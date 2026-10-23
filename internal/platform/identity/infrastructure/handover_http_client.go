package infrastructure

import (
	"bytes"
	"context"
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

// HandoverEndpointError 表示"没有可用结论"的传输层失败（网络、超时、5xx），
// 平台必须据此走"先查回执再退避重试"，而不是当成业务失败。
type HandoverEndpointError struct {
	System string
	Reason string
}

// Error 输出系统名与原因摘要；原因只是固定分类词，不携带网络细节或凭据。
func (err *HandoverEndpointError) Error() string {
	return fmt.Sprintf("handover endpoint %s unavailable: %s", err.System, err.Reason)
}

// HandoverHTTPClient 是业务系统机器协议的客户端实现。它只使用内网地址与最小权限
// 机器凭据，并把每次写请求的幂等键与防重放标识原样透传。
type HandoverHTTPClient struct {
	system          string
	baseURL         string
	machineToken    string
	tenantID        string
	applicationCode string
	environment     string
	client          *http.Client
	requestID       func() (string, error)
}

// HandoverHTTPClientConfig 汇总机器协议客户端的端点、凭据与租户边界；MachineToken 只在
// 内存中持有，不会进入错误消息或日志。
type HandoverHTTPClientConfig struct {
	System          string
	BaseURL         string
	MachineToken    string
	TenantID        string
	ApplicationCode string
	Environment     string
	Timeout         time.Duration
	RequestID       func() (string, error)
}

// NewHandoverHTTPClient 构造协议客户端。系统名、内网 BaseURL、机器凭据与请求 ID 生成器
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
	if strings.TrimSpace(config.MachineToken) == "" {
		return nil, errors.New("handover http client requires a machine credential")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	requestID := config.RequestID
	if requestID == nil {
		return nil, errors.New("handover http client requires a request id generator")
	}
	return &HandoverHTTPClient{
		system: system, baseURL: baseURL, machineToken: config.MachineToken,
		tenantID: config.TenantID, applicationCode: config.ApplicationCode, environment: config.Environment,
		client: &http.Client{Timeout: timeout}, requestID: requestID,
	}, nil
}

// Freeze 请求业务系统冻结该交接批次涉及的资源；请求 ID 透传给对端做防重放关联，
// 冻结结果与责任数量由对端权威返回。
func (client *HandoverHTTPClient) Freeze(ctx context.Context, request application.HandoverFreezeRequest) (application.HandoverFreezeResponse, error) {
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
	if err := client.do(ctx, http.MethodPost, "/internal/handover/freeze", request.RequestID, "", body, &response); err != nil {
		return application.HandoverFreezeResponse{}, err
	}
	return application.HandoverFreezeResponse{System: client.orSystem(response.System), Frozen: response.Frozen, FrozenAt: response.FrozenAt, ResponsibilityCount: response.ResponsibilityCount}, nil
}

// Responsibilities 分页拉取冻结后发现的权威责任清单；cursor/limit 语义由对端定义，
// 平台不做本地截断，避免两个系统对“同一页”的理解出现偏差。
func (client *HandoverHTTPClient) Responsibilities(ctx context.Context, requestID, cursor string, limit int) (application.HandoverResponsibilityPage, error) {
	query := url.Values{}
	query.Set("request_id", requestID)
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
	if err := client.do(ctx, http.MethodGet, "/internal/handover/responsibilities?"+query.Encode(), requestID, "", nil, &response); err != nil {
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
	body := map[string]any{
		"tenant_id": request.TenantID, "batch_id": request.BatchID, "item_id": request.ItemID, "request_id": request.RequestID,
		"resource_type": request.ResourceType, "resource_id": request.ResourceID,
		"from_user_id": request.FromUserID, "to_user_id": request.ToUserID,
		"expected_version": request.ExpectedVersion, "idempotency_key": request.IdempotencyKey,
		"lease_id": request.LeaseID, "deadline": request.Deadline.UTC().Format(time.RFC3339Nano),
	}
	var response handoverReceiptPayload
	if err := client.do(ctx, http.MethodPost, "/internal/handover/transfers", request.RequestID, request.IdempotencyKey, body, &response); err != nil {
		return application.HandoverTransferReceipt{}, err
	}
	return client.toReceipt(response), nil
}

// Receipt 按条目 ID 与幂等键查询对端已落库的转移回执，用于“结果未知”后的先查证再重试；
// 路径与查询参数做 URL 转义，防止 ID 拼接改变请求目标。
func (client *HandoverHTTPClient) Receipt(ctx context.Context, itemID, idempotencyKey string) (application.HandoverTransferReceipt, error) {
	path := "/internal/handover/transfers/" + url.PathEscape(itemID) + "?idempotency_key=" + url.QueryEscape(idempotencyKey)
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
// 不再接受新转移。
func (client *HandoverHTTPClient) Finish(ctx context.Context, batchID, requestID, outcome string) error {
	body := map[string]any{"tenant_id": client.tenantID, "batch_id": batchID, "request_id": requestID, "outcome": outcome}
	return client.do(ctx, http.MethodPost, "/internal/handover/complete", requestID, "", body, nil)
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

// do 发送一次机器协议请求。写请求一定携带 Idempotency-Key 与 X-Request-Id；
// 只有 4xx 才是确定性业务失败，其余（含 5xx）都按"结果未知"处理。
func (client *HandoverHTTPClient) do(ctx context.Context, method, path, requestID, idempotencyKey string, body any, out any) error {
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
	request.Header.Set("Authorization", "Bearer "+client.machineToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(client.tenantID) != "" {
		request.Header.Set("X-Tenant-Id", client.tenantID)
	}
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
