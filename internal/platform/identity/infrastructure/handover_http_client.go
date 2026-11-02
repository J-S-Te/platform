package infrastructure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
)

// 交接机器协议端点统一挂在 /internal/v1/handover 下；四个业务系统实现同一组路径，
// 平台不再兼容早期 /internal/handover 前缀（历史差异见 BIZ-XWIRE-04）。
const (
	handoverFreezePath           = "/internal/v1/handover/freeze"
	handoverResponsibilitiesPath = "/internal/v1/handover/responsibilities"
	handoverTransfersPath        = "/internal/v1/handover/transfers"
	handoverCompletePath         = "/internal/v1/handover/complete"
	// handoverNonceBytes 是防重放 nonce 的熵大小；对端按 base64url 解码后必须等长。
	handoverNonceBytes = 32
)

// HandoverProtocolError 表示机器协议对端给出的确定性失败；它只携带对端返回的
// 业务错误码，绝不携带机器凭据、URL 凭据或响应正文片段。
type HandoverProtocolError struct {
	System     string
	StatusCode int
	Code       string
	Message    string
}

// Error 只输出系统名、状态码与业务错误码的组合文本，便于日志定位且不包含对端响应正文。
func (err *HandoverProtocolError) Error() string {
	return fmt.Sprintf("handover protocol error on %s: status=%d code=%s", err.System, err.StatusCode, err.Code)
}

// HandoverEndpointError 表示"没有可用结论"的传输层失败（网络、超时、5xx、凭据铸造失败），
// 平台必须据此走"先查回执再退避重试"，而不是当成业务失败。
type HandoverEndpointError struct {
	System string
	Reason string
}

// Error 输出系统名与原因摘要；原因只是固定分类词，不携带网络细节或凭据。
func (err *HandoverEndpointError) Error() string {
	return fmt.Sprintf("handover endpoint %s unavailable: %s", err.System, err.Reason)
}

// HandoverHTTPClient 是业务系统机器协议的客户端实现。它只使用内网地址，并按当前批次
// 的租户向平台自己的 ApplicationJWTManager 换取短效 application JWT；客户端自身不保存
// 任何长期机器令牌，写请求的幂等键与防重放标识原样透传。
type HandoverHTTPClient struct {
	system          string
	baseURL         string
	tokenSource     func(tenantID string) (string, error)
	applicationCode string
	environment     string
	client          *http.Client
	requestID       func() (string, error)
	now             func() time.Time
}

// HandoverHTTPClientConfig 汇总机器协议客户端的端点与令牌来源；TokenSource 必须按
// 租户铸造短效令牌，客户端只持有函数引用，不缓存令牌，也不进入错误消息或日志。
type HandoverHTTPClientConfig struct {
	System  string
	BaseURL string
	// TokenSource 按当前批次租户铸造 application JWT；返回错误或空令牌时本次请求不发送。
	TokenSource     func(tenantID string) (string, error)
	ApplicationCode string
	Environment     string
	Timeout         time.Duration
	RequestID       func() (string, error)
	// Now 用于请求时间戳；缺省使用系统时钟（测试可注入固定时刻）。
	Now func() time.Time
}

// NewHandoverHTTPClient 构造协议客户端。系统名、内网 BaseURL、令牌来源与请求 ID 生成器
// 为必填，缺失即拒绝；Timeout 非正数时回落到 10 秒默认值，保证调用总有界。
func NewHandoverHTTPClient(config HandoverHTTPClientConfig) (*HandoverHTTPClient, error) {
	system := strings.TrimSpace(config.System)
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if system == "" || baseURL == "" {
		return nil, errors.New("handover http client requires system and base url")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("handover base url is invalid: %w", err)
	}
	if config.TokenSource == nil {
		return nil, errors.New("handover http client requires an application token source")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	requestID := config.RequestID
	if requestID == nil {
		return nil, errors.New("handover http client requires a request id generator")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &HandoverHTTPClient{
		system: system, baseURL: baseURL, tokenSource: config.TokenSource,
		applicationCode: config.ApplicationCode, environment: config.Environment,
		client: &http.Client{Timeout: timeout}, requestID: requestID, now: now,
	}, nil
}

// Freeze 请求业务系统冻结该交接批次涉及的资源；请求 ID 透传给对端做防重放关联，
// 冻结结果与责任数量由对端权威返回。
func (client *HandoverHTTPClient) Freeze(ctx context.Context, request application.HandoverFreezeRequest) (application.HandoverFreezeResponse, error) {
	ctx = application.WithHandoverTenant(ctx, request.TenantID)
	body := map[string]any{
		"tenant_id": request.TenantID, "batch_id": request.BatchID, "request_id": request.RequestID,
		"user_id": request.UserID, "requested_at": request.RequestedAt.UTC().Format(time.RFC3339Nano),
	}
	var response struct {
		System              string     `json:"system"`
		Frozen              bool       `json:"frozen"`
		FrozenAt            *time.Time `json:"frozen_at"`
		ResponsibilityCount int        `json:"responsibility_count"`
	}
	if err := client.do(ctx, http.MethodPost, handoverFreezePath, request.RequestID, "", body, &response); err != nil {
		return application.HandoverFreezeResponse{}, err
	}
	return application.HandoverFreezeResponse{System: client.orSystem(response.System), Frozen: response.Frozen, FrozenAt: response.FrozenAt, ResponsibilityCount: response.ResponsibilityCount}, nil
}

// Responsibilities 分页拉取冻结后发现的权威责任清单；cursor/limit 语义由对端定义，
// 平台不做本地截断，避免两个系统对"同一页"的理解出现偏差。
//
// userID 是当前批次的离职人（人员异动请求的 userID）。合同、CRM、项目、结算四端的
// GET /internal/v1/handover/responsibilities 都以 user_id 查询参数为必填，缺失即返回
// 422 并让整批 discovery 失败，因此空 userID 在本地失败关闭：不发请求。
func (client *HandoverHTTPClient) Responsibilities(ctx context.Context, requestID, userID, cursor string, limit int) (application.HandoverResponsibilityPage, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		// 四端契约要求 user_id；空值必然被 422 拒绝，本地不发送并给出可判定的校验错误。
		return application.HandoverResponsibilityPage{}, fmt.Errorf("handover %s responsibilities requires the batch user id: %w", client.system, application.ErrValidation)
	}
	query := url.Values{}
	query.Set("request_id", requestID)
	// user_id 是对端权威过滤条件：平台只能按离职人索取责任，不能索取全租户清单。
	query.Set("user_id", userID)
	if strings.TrimSpace(cursor) != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var response struct {
		System     string `json:"system"`
		NextCursor string `json:"next_cursor"`
		Items      []struct {
			ResourceType string    `json:"resource_type"`
			ResourceID   string    `json:"resource_id"`
			ResourceName string    `json:"resource_name"`
			OwnerUserID  string    `json:"owner_user_id"`
			Category     string    `json:"category"`
			Version      uint64    `json:"version"`
			UpdatedAt    time.Time `json:"updated_at"`
		} `json:"items"`
	}
	if err := client.do(ctx, http.MethodGet, handoverResponsibilitiesPath+"?"+query.Encode(), requestID, "", nil, &response); err != nil {
		return application.HandoverResponsibilityPage{}, err
	}
	page := application.HandoverResponsibilityPage{System: client.orSystem(response.System), NextCursor: response.NextCursor}
	for _, item := range response.Items {
		page.Items = append(page.Items, application.HandoverResponsibility{
			System: client.system, ResourceType: item.ResourceType, ResourceID: item.ResourceID,
			ResourceName: item.ResourceName, OwnerUserID: item.OwnerUserID, Category: item.Category,
			Version: item.Version, UpdatedAt: item.UpdatedAt,
		})
	}
	return page, nil
}

// Transfer 转移单个责任并携带幂等键、租约 ID 与截止时间；只有对端回执才构成转移证据，
// 传输层失败不代表业务失败，必须先查回执再重试。
func (client *HandoverHTTPClient) Transfer(ctx context.Context, request application.HandoverTransferRequest) (application.HandoverTransferReceipt, error) {
	ctx = application.WithHandoverTenant(ctx, request.TenantID)
	body := map[string]any{
		"tenant_id": request.TenantID, "batch_id": request.BatchID, "item_id": request.ItemID, "request_id": request.RequestID,
		"resource_type": request.ResourceType, "resource_id": request.ResourceID,
		"from_user_id": request.FromUserID, "to_user_id": request.ToUserID,
		"expected_version": request.ExpectedVersion, "idempotency_key": request.IdempotencyKey,
		"lease_id": request.LeaseID, "deadline": request.Deadline.UTC().Format(time.RFC3339Nano),
	}
	var response handoverReceiptPayload
	if err := client.do(ctx, http.MethodPost, handoverTransfersPath, request.RequestID, request.IdempotencyKey, body, &response); err != nil {
		return application.HandoverTransferReceipt{}, err
	}
	return client.toReceipt(response), nil
}

// Receipt 按条目 ID 与幂等键查询对端已落库的转移回执，用于"结果未知"后的先查证再重试；
// 路径与查询参数做 URL 转义，防止 ID 拼接改变请求目标。
func (client *HandoverHTTPClient) Receipt(ctx context.Context, itemID, idempotencyKey string) (application.HandoverTransferReceipt, error) {
	path := handoverTransfersPath + "/" + url.PathEscape(itemID) + "?idempotency_key=" + url.QueryEscape(idempotencyKey)
	var response handoverReceiptPayload
	if err := client.do(ctx, http.MethodGet, path, "", "", nil, &response); err != nil {
		var protocolErr *HandoverProtocolError
		if errors.As(err, &protocolErr) && protocolErr.StatusCode == http.StatusNotFound {
			return application.HandoverTransferReceipt{}, application.ErrNotFound
		}
		return application.HandoverTransferReceipt{}, err
	}
	return client.toReceipt(response), nil
}

// Finish 通知对端批次收口（完成或取消）。取消不要求对端回滚已完成的转移，只声明批次
// 不再接受新转移；租户同样只来自当前批次的上下文。
func (client *HandoverHTTPClient) Finish(ctx context.Context, batchID, requestID, outcome string) error {
	tenantID, ok := application.HandoverTenantFromContext(ctx)
	if !ok {
		return fmt.Errorf("handover %s finish requires a tenant context", client.system)
	}
	body := map[string]any{"tenant_id": tenantID, "batch_id": batchID, "request_id": requestID, "outcome": outcome}
	return client.do(ctx, http.MethodPost, handoverCompletePath, requestID, "", body, nil)
}

type handoverReceiptPayload struct {
	System         string    `json:"system"`
	ItemID         string    `json:"item_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Status         string    `json:"status"`
	AppliedVersion uint64    `json:"applied_version"`
	ReasonCode     string    `json:"reason_code"`
	EvidenceRef    string    `json:"evidence_ref"`
	ReceivedAt     time.Time `json:"received_at"`
}

func (client *HandoverHTTPClient) toReceipt(payload handoverReceiptPayload) application.HandoverTransferReceipt {
	return application.HandoverTransferReceipt{
		System: client.orSystem(payload.System), ItemID: payload.ItemID, IdempotencyKey: payload.IdempotencyKey,
		Status: payload.Status, AppliedVersion: payload.AppliedVersion, ReasonCode: payload.ReasonCode,
		EvidenceRef: payload.EvidenceRef, ReceivedAt: payload.ReceivedAt,
	}
}

func (client *HandoverHTTPClient) orSystem(value string) string {
	if strings.TrimSpace(value) == "" {
		return client.system
	}
	return value
}

// do 发送一次机器协议请求。租户必须由编排器写入上下文：令牌按租户铸造，铸造失败时
// 本次请求不发送；写请求一定携带 Idempotency-Key、X-Request-Id、时间戳与一次性 nonce，
// 只有 4xx 才是确定性业务失败，其余（含 5xx）都按"结果未知"处理。
func (client *HandoverHTTPClient) do(ctx context.Context, method, path, requestID, idempotencyKey string, body any, out any) error {
	tenantID, ok := application.HandoverTenantFromContext(ctx)
	if !ok {
		return fmt.Errorf("handover %s request requires a tenant context", client.system)
	}
	token, tokenErr := client.tokenSource(tenantID)
	if tokenErr != nil || strings.TrimSpace(token) == "" {
		// 凭据铸造失败属于"没有可用结论"：不发送请求，也不能把原因细节带回调用方。
		return &HandoverEndpointError{System: client.system, Reason: "credential unavailable"}
	}
	nonce, nonceErr := handoverNonce()
	if nonceErr != nil {
		return fmt.Errorf("generate handover nonce: %w", nonceErr)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode handover request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build handover request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// 对端（尤其 CRM）的机器入口要求时间戳 + 一次性 nonce；缺失会在验签后被拒绝。
	request.Header.Set("X-Integration-Timestamp", client.now().UTC().Format(time.RFC3339Nano))
	request.Header.Set("X-Integration-Nonce", nonce)
	request.Header.Set("X-Tenant-Id", tenantID)
	if strings.TrimSpace(client.applicationCode) != "" {
		request.Header.Set("X-Application-Code", client.applicationCode)
	}
	if strings.TrimSpace(client.environment) != "" {
		request.Header.Set("X-Environment", client.environment)
	}
	traceID := strings.TrimSpace(requestID)
	if traceID == "" {
		generated, genErr := client.requestID()
		if genErr != nil {
			return fmt.Errorf("generate handover request id: %w", genErr)
		}
		traceID = generated
	}
	request.Header.Set("X-Request-Id", traceID)
	if strings.TrimSpace(idempotencyKey) != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.client.Do(request)
	if err != nil {
		// 传输层失败不携带任何凭据或 URL 细节。
		return &HandoverEndpointError{System: client.system, Reason: "transport failure"}
	}
	defer func() { _ = response.Body.Close() }()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return &HandoverEndpointError{System: client.system, Reason: "response read failure"}
	}
	if response.StatusCode >= 400 {
		if response.StatusCode >= 500 {
			return &HandoverEndpointError{System: client.system, Reason: "remote server error"}
		}
		return &HandoverProtocolError{System: client.system, StatusCode: response.StatusCode, Code: errorCodeOf(payload)}
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	// 业务系统普遍返回 {code,message,data} 信封，也有实现直接返回 DTO；
	// 两种形态都必须能解码，否则跨系统联调会读到零值而误判成功/失败。
	decoded := payload
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && len(envelope.Data) > 0 {
		decoded = envelope.Data
	}
	if err := json.Unmarshal(decoded, out); err != nil {
		// 对端返回了平台无法解释的结构：按接口不兼容处理，不能猜。
		return &HandoverProtocolError{System: client.system, StatusCode: response.StatusCode, Code: "INCOMPATIBLE_RESPONSE"}
	}
	return nil
}

// handoverNonce 生成对端可用于防重放的一次性标识（32 字节随机数，base64url 无填充）。
func handoverNonce() (string, error) {
	nonce := make([]byte, handoverNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(nonce), nil
}

// errorCodeOf 只提取对端声明的业务错误码，绝不回传正文片段。
func errorCodeOf(payload []byte) string {
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || strings.TrimSpace(envelope.Code) == "" {
		return "PROTOCOL_ERROR"
	}
	return strings.TrimSpace(envelope.Code)
}
