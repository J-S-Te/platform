// Package http 通过统一响应信封提供审计写入和只读控制台接口。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

const maxRequestBytes = 256 << 10

// Handler 适配审计用例到统一 HTTP 响应协议；租户范围取自认证上下文，
// 导出下载受 storageRoot 约束。
type Handler struct {
	service     *application.Service
	logger      *slog.Logger
	storageRoot string
}

// NewHandler 构造审计 HTTP 处理器；任一依赖缺失时失败关闭。
func NewHandler(service *application.Service, logger *slog.Logger, storageRoot string) (*Handler, error) {
	if service == nil || logger == nil || strings.TrimSpace(storageRoot) == "" {
		return nil, errors.New("audit HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger, storageRoot: filepath.Clean(storageRoot)}, nil
}

type eventInputPayload struct {
	EventID         string    `json:"event_id"`
	ApplicationCode string    `json:"application_code"`
	EnvironmentCode string    `json:"environment_code"`
	ActorType       string    `json:"actor_type,omitempty"`
	ActorID         string    `json:"actor_id,omitempty"`
	ActorName       string    `json:"actor_name,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
	Action          string    `json:"action"`
	ResourceType    string    `json:"resource_type"`
	ResourceID      string    `json:"resource_id,omitempty"`
	ResourceName    string    `json:"resource_name,omitempty"`
	BusinessID      string    `json:"business_id,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	TraceID         string    `json:"trace_id,omitempty"`
	CorrelationID   string    `json:"correlation_id,omitempty"`
	Result          string    `json:"result"`
	ReasonCode      string    `json:"reason_code,omitempty"`
	RiskLevel       string    `json:"risk_level,omitempty"`
	Classification  string    `json:"classification,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	// 统一审计来源协议（AUD-IP-2026）：
	//   login_ip  创建应用会话时观察到的客户端地址（登录态固定）
	//   client_ip 本次业务操作的客户端地址
	//   ip_source 地址来源标记：DIRECT / TRUSTED_PROXY / LEGACY_PROTOCOL
	// 三者都必须由发布方在可信入口解析后填写，平台不接受来自不可信代理头的推断。
	LoginIP  string `json:"login_ip,omitempty"`
	ClientIP string `json:"client_ip,omitempty"`
	IPSource string `json:"ip_source,omitempty"`
	// UserLoginIP 是旧协议的兼容字段：只填登录地址，缺省时按 LEGACY_PROTOCOL 处理。
	UserLoginIP   string               `json:"user_login_ip,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
	ChangeSummary []domain.FieldChange `json:"change_summary,omitempty"`
}

type batchPayload struct {
	Events []eventInputPayload `json:"events"`
}

type exportPayload struct {
	ApplicationCode string     `json:"application_code,omitempty"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	Action          string     `json:"action,omitempty"`
	ActionCategory  string     `json:"action_category,omitempty"`
	Result          string     `json:"result,omitempty"`
	RiskLevel       string     `json:"risk_level,omitempty"`
	Keyword         string     `json:"keyword,omitempty"`
	OccurredFrom    *time.Time `json:"occurred_from,omitempty"`
	OccurredTo      *time.Time `json:"occurred_to,omitempty"`
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}
type eventResponse struct {
	EventID             string               `json:"event_id"`
	OccurredAt          time.Time            `json:"occurred_at"`
	OperatorDisplayName string               `json:"operator_display_name"`
	ActionType          string               `json:"action_type"`
	ApplicationCode     string               `json:"application_code"`
	ApplicationName     string               `json:"application_name"`
	EnvironmentCode     string               `json:"environment_code"`
	Action              string               `json:"action"`
	Result              string               `json:"result"`
	ResourceType        string               `json:"resource_type"`
	ResourceID          string               `json:"resource_id,omitempty"`
	ResourceName        string               `json:"resource_name,omitempty"`
	Method              string               `json:"method,omitempty"`
	Path                string               `json:"path,omitempty"`
	ClientIP            string               `json:"client_ip,omitempty"`
	LoginIP             string               `json:"login_ip,omitempty"`
	IngestPeerIP        string               `json:"ingest_peer_ip,omitempty"`
	IPSource            string               `json:"ip_source,omitempty"`
	UserAgent           string               `json:"user_agent,omitempty"`
	RequestID           string               `json:"request_id,omitempty"`
	TraceID             string               `json:"trace_id,omitempty"`
	CorrelationID       string               `json:"correlation_id,omitempty"`
	StatusCode          int                  `json:"status_code,omitempty"`
	RiskLevel           string               `json:"risk_level"`
	Detail              string               `json:"detail,omitempty"`
	Summary             string               `json:"summary,omitempty"`
	ChangeSummary       []domain.FieldChange `json:"change_summary"`
}
type exportResponse struct {
	JobID        string     `json:"job_id"`
	Status       string     `json:"status"`
	DownloadURL  string     `json:"download_url,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// ingestionValidationResponse 有意不含任何凭据。它让审计发布方证明平台已接受其
// bearer token 与来源绑定，而不暴露任何令牌、密钥或授权内部细节。
type ingestionValidationResponse struct {
	Status          string `json:"status"`
	ApplicationCode string `json:"application_code"`
	EnvironmentCode string `json:"environment_code"`
	ClientID        string `json:"client_id"`
	AuditIngest     bool   `json:"audit_ingest"`
}

// ListEvents 分页返回当前租户的审计事件。
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.List(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]eventResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, eventToResponse(item))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计事件查询成功", pageResponse[eventResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}
// Ingest 接收应用机器主体提交的单个审计事件；租户与来源取自已验签凭据。
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.applicationPrincipal(w, r)
	if !ok {
		return
	}
	var payload eventInputPayload
	if !decode(w, r, &payload) {
		return
	}
	if err := validateExternalCorrelation(r.Context(), payload, true); err != nil {
		h.writeError(w, r, err)
		return
	}
	input, err := applicationInput(payload, r, principal)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	receipt, err := h.service.Ingest(r.Context(), principal.TenantID, input)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "审计事件已接收", receipt)
}
// IngestBatch 接收有界批量审计事件；整批校验通过后才持久化，重复事件按幂等成功返回。
func (h *Handler) IngestBatch(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.applicationPrincipal(w, r)
	if !ok {
		return
	}
	var payload batchPayload
	if !decode(w, r, &payload) {
		return
	}
	inputs := make([]application.EventInput, 0, len(payload.Events))
	for _, event := range payload.Events {
		if err := validateExternalCorrelation(r.Context(), event, false); err != nil {
			h.writeError(w, r, err)
			return
		}
		input, err := applicationInput(event, r, principal)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		inputs = append(inputs, input)
	}
	delivery := batchDeliveryInput(r, principal)
	receipts, err := h.service.IngestBatch(r.Context(), principal.TenantID, delivery, inputs)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "审计事件已接收", receipts)
}

// ValidateIngestion 在与摄取相同的边界上校验 OAuth 应用主体。有意只读且不创建
// 审计回执，使部署探针能在 Outbox 开始堆积重试之前发现应用/环境/client 不匹配。
func (h *Handler) ValidateIngestion(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.applicationPrincipal(w, r)
	if !ok {
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计接入配置校验通过", ingestionValidationResponse{
		Status:          "READY",
		ApplicationCode: principal.ApplicationCode,
		EnvironmentCode: principal.EnvironmentCode,
		ClientID:        principal.ClientID,
		AuditIngest:     principal.HasScope("audit.ingest"),
	})
}
// GetEvent 返回租户内单个审计事件详情。
func (h *Handler) GetEvent(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	event, err := h.service.Get(r.Context(), principal.Tenant.ID, r.PathValue("event_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计事件查询成功", eventToResponse(event))
}

// IngestionHealth 返回平台侧审计摄取健康度（批次 3）。
//
// 只读、租户隔离；用于审计页的"摄取健康"卡片，口径是平台已接收的事件，
// 不代表各子系统本地 Outbox 的积压量（那些库平台读不到）。
func (h *Handler) IngestionHealth(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	health, err := h.service.IngestionHealth(r.Context(), principal.Tenant.ID, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计摄取健康度查询成功", health)
}

// CreateExportJob 以当前查询条件创建异步审计导出任务。
func (h *Handler) CreateExportJob(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload exportPayload
	if !decode(w, r, &payload) {
		return
	}
	job, err := h.service.CreateExportJob(r.Context(), principal.Tenant.ID, principal.User.ID, application.PageRequest{Keyword: payload.Keyword, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.EnvironmentCode, Action: payload.Action, ActionCategory: payload.ActionCategory, Result: payload.Result, RiskLevel: payload.RiskLevel, OccurredFrom: payload.OccurredFrom, OccurredTo: payload.OccurredTo})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "审计导出任务已创建", exportToResponse(job))
}

// DownloadExport 从本地存储根流式返回一个已完成的租户隔离导出。
// 绝不从请求接受存储路径，并在打开文件前校验路径合法性。
func (h *Handler) DownloadExport(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	file, err := h.service.GetExportFile(r.Context(), principal.Tenant.ID, r.PathValue("job_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	absolutePath, err := storagePath(h.storageRoot, file.StorageRelativePath)
	if err != nil {
		h.logger.Error("resolve audit export path", "job_id", r.PathValue("job_id"), "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
		return
	}
	opened, err := os.Open(absolutePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
			return
		}
		h.logger.Error("open audit export", "job_id", r.PathValue("job_id"), "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
		return
	}
	defer opened.Close()

	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() {
		h.logger.Error("inspect audit export", "job_id", r.PathValue("job_id"), "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
		return
	}
	mediaType := strings.TrimSpace(file.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.OriginalName}))
	http.ServeContent(w, r, file.OriginalName, info.ModTime(), opened)
}

// GetExportJob 返回导出任务状态与下载地址。
func (h *Handler) GetExportJob(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	job, err := h.service.GetExportJob(r.Context(), principal.Tenant.ID, r.PathValue("job_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计导出任务查询成功", exportToResponse(job))
}
func (h *Handler) applicationPrincipal(w http.ResponseWriter, r *http.Request) (appctx.Principal, bool) {
	principal, ok := appctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return principal, ok
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return principal, ok
}
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	default:
		h.logger.Error("audit request failed", "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}
func pageQuery(r *http.Request) application.PageRequest {
	values := r.URL.Query()
	query := application.PageRequest{Page: parseInt(values.Get("page"), 1), PageSize: parseInt(values.Get("page_size"), 20), Keyword: strings.TrimSpace(values.Get("keyword")), ApplicationCode: strings.TrimSpace(filterValue(values, "application_code")), EnvironmentCode: strings.TrimSpace(filterValue(values, "environment_code")), Action: strings.TrimSpace(filterValue(values, "action")), ActionCategory: strings.TrimSpace(filterValue(values, "action_category")), Result: strings.TrimSpace(filterValue(values, "result")), RiskLevel: strings.TrimSpace(filterValue(values, "risk_level")), ClientIP: strings.TrimSpace(filterValue(values, "client_ip")), ResourceID: strings.TrimSpace(filterValue(values, "resource_id")), RequestID: strings.TrimSpace(filterValue(values, "request_id"))}
	query.OccurredFrom = parseTime(filterValue(values, "occurred_from"))
	query.OccurredTo = parseTime(filterValue(values, "occurred_to"))
	return query
}

// filterValue 兼容文档化的 filter[field] 语法与旧版前端包使用的扁平字段写法。
// 同时支持两者，避免滚动发布期间缓存混布时静默丢失过滤条件。
func filterValue(values url.Values, name string) string {
	if value := values.Get("filter[" + name + "]"); value != "" {
		return value
	}
	return values.Get(name)
}

// applicationInput 要求来自应用 bearer token 的显式来源绑定。外部载荷永远不能控制
// client_id：它只从已验证主体注入。
func applicationInput(payload eventInputPayload, r *http.Request, principal appctx.Principal) (application.EventInput, error) {
	if payload.ApplicationCode == "" || strings.TrimSpace(payload.ApplicationCode) != payload.ApplicationCode || payload.ApplicationCode != principal.ApplicationCode {
		return application.EventInput{}, fmt.Errorf("%w: application_code must be non-empty and exactly match access token", application.ErrValidation)
	}
	if payload.EnvironmentCode == "" || strings.TrimSpace(payload.EnvironmentCode) != payload.EnvironmentCode || payload.EnvironmentCode != principal.EnvironmentCode {
		return application.EventInput{}, fmt.Errorf("%w: environment_code must be non-empty and exactly match access token", application.ErrValidation)
	}
	if strings.TrimSpace(principal.ClientID) == "" {
		return application.EventInput{}, fmt.Errorf("%w: application token client_id is required", application.ErrValidation)
	}

	input, err := toInput(payload, r)
	if err != nil {
		return application.EventInput{}, err
	}
	input.ClientID = principal.ClientID
	return input, nil
}

// batchDeliveryInput 捕获一次 Outbox 投递的可信传输元数据。这些值与每条事件原有的
// 操作级关联字段分开存储。
func batchDeliveryInput(r *http.Request, principal appctx.Principal) application.BatchDeliveryInput {
	return application.BatchDeliveryInput{
		ApplicationCode: principal.ApplicationCode,
		EnvironmentCode: principal.EnvironmentCode,
		ClientID:        principal.ClientID,
		RequestID:       requestctx.RequestID(r.Context()),
		TraceID:         requestctx.TraceID(r.Context()),
		CorrelationID:   requestctx.CorrelationID(r.Context()),
		SourceIP:        clientIP(r),
		UserAgent:       r.UserAgent(),
	}
}

// validateExternalCorrelation 强制执行已发布的子系统审计契约。单事件请求必须在
// 请求体中重复可信传输三元组；批量请求保留一个投递级传输三元组，每条事件
// 各自携带原有的操作级三元组。
func validateExternalCorrelation(ctx context.Context, payload eventInputPayload, matchTransport bool) error {
	if strings.TrimSpace(payload.ActorType) == "" || strings.TrimSpace(payload.RequestID) == "" || strings.TrimSpace(payload.TraceID) == "" || strings.TrimSpace(payload.CorrelationID) == "" {
		return fmt.Errorf("%w: actor_type, request_id, trace_id and correlation_id are required", application.ErrValidation)
	}
	if !matchTransport {
		return nil
	}
	if strings.TrimSpace(payload.RequestID) != requestctx.RequestID(ctx) ||
		strings.TrimSpace(payload.TraceID) != requestctx.TraceID(ctx) ||
		strings.TrimSpace(payload.CorrelationID) != requestctx.CorrelationID(ctx) {
		return fmt.Errorf("%w: audit correlation fields do not match transport headers", application.ErrValidation)
	}
	return nil
}

func toInput(payload eventInputPayload, r *http.Request) (application.EventInput, error) {
	metadata := copyMetadata(payload.Metadata)
	// method 与 path 可以描述被审计的子系统操作。保留上报方提供的值，仅在
	// 上报应用未提供时补充接收侧的值。
	if _, ok := metadata["method"]; !ok {
		metadata["method"] = r.Method
	}
	if _, ok := metadata["path"]; !ok {
		metadata["path"] = r.URL.Path
	}
	loginIP, clientIP, sourceFlag, legacySourceIP, err := resolveEventIPs(payload, r)
	if err != nil {
		return application.EventInput{}, err
	}
	return application.EventInput{LoginIP: loginIP, ClientIP: clientIP, IngestPeerIP: deliveryPeerIP(r), IPSource: sourceFlag, EventID: payload.EventID, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.EnvironmentCode, ActorType: payload.ActorType, ActorID: payload.ActorID, ActorName: payload.ActorName, SessionID: payload.SessionID, OccurredAt: payload.OccurredAt, Action: payload.Action, ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, ResourceName: payload.ResourceName, BusinessID: payload.BusinessID, RequestID: payload.RequestID, TraceID: payload.TraceID, CorrelationID: payload.CorrelationID, Result: payload.Result, ReasonCode: payload.ReasonCode, RiskLevel: payload.RiskLevel, Classification: payload.Classification, Summary: payload.Summary, Metadata: metadata, Changes: payload.ChangeSummary, SourceIP: legacySourceIP, UserAgent: r.UserAgent()}, nil
}

// resolveEventIPs 依据统一审计来源协议解析三类地址。
//
// 规则：
//   - 新协议字段（login_ip / client_ip）优先，必须是合法 IP 字面量；
//   - 只提供旧字段 user_login_ip 时按 LEGACY_PROTOCOL 处理，并只当作"登录地址"；
//   - 什么都没提供时登录/操作地址保持空，由界面显示"未采集"，绝不用投递地址冒充；
//   - legacySourceIP 仅用于兼容旧列 source_ip，保持历史读数的连续性。
func resolveEventIPs(payload eventInputPayload, r *http.Request) (loginIP, clientIP, ipSource, legacySourceIP string, err error) {
	peer := deliveryPeerIP(r)
	parse := func(field, value string) (string, error) {
		if strings.TrimSpace(value) != value || value == "" {
			return "", fmt.Errorf("%w: %s must be a valid IP address", application.ErrValidation, field)
		}
		parsed := net.ParseIP(value)
		if parsed == nil {
			return "", fmt.Errorf("%w: %s must be a valid IP address", application.ErrValidation, field)
		}
		return parsed.String(), nil
	}
	ipSource = normalizeAuditIPSource(payload.IPSource)
	if payload.LoginIP != "" {
		if loginIP, err = parse("login_ip", payload.LoginIP); err != nil {
			return "", "", "", "", err
		}
	}
	if payload.ClientIP != "" {
		if clientIP, err = parse("client_ip", payload.ClientIP); err != nil {
			return "", "", "", "", err
		}
	}
	if loginIP == "" && clientIP == "" && payload.UserLoginIP != "" {
		// 旧发布方只上报登录地址。
		if loginIP, err = parse("user_login_ip", payload.UserLoginIP); err != nil {
			return "", "", "", "", err
		}
		ipSource = "LEGACY_PROTOCOL"
	}
	if loginIP == "" && clientIP == "" && payload.UserLoginIP == "" {
		ipSource = "UNKNOWN"
	}
	// 兼容列：保持历史语义（登录地址优先，其次投递地址），供仍在读取 source_ip 的旧消费方使用。
	legacySourceIP = loginIP
	if legacySourceIP == "" {
		legacySourceIP = peer
	}
	return loginIP, clientIP, ipSource, legacySourceIP, nil
}

// normalizeAuditIPSource 收敛发布方上报的来源标记；未知取值记为 UNKNOWN。
func normalizeAuditIPSource(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DIRECT":
		return "DIRECT"
	case "TRUSTED_PROXY":
		return "TRUSTED_PROXY"
	case "LEGACY_PROTOCOL":
		return "LEGACY_PROTOCOL"
	default:
		return "UNKNOWN"
	}
}

// deliveryPeerIP 只描述"谁把审计送来"：可信代理解析出的请求地址，缺省回退连接地址。
func deliveryPeerIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if trusted := strings.TrimSpace(requestctx.ClientIP(r.Context())); net.ParseIP(trusted) != nil {
		return net.ParseIP(trusted).String()
	}
	return clientIP(r)
}

func copyMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return map[string]any{}
	}
	copy := make(map[string]any, len(metadata)+2)
	for key, value := range metadata {
		copy[key] = value
	}
	return copy
}
func eventToResponse(event domain.Event) eventResponse {
	return eventResponse{EventID: event.EventID, OccurredAt: event.OccurredAt, OperatorDisplayName: event.OperatorDisplayName, ActionType: event.ActionType, ApplicationCode: event.ApplicationCode, ApplicationName: event.ApplicationName, EnvironmentCode: event.EnvironmentCode, Action: event.Action, Result: event.Result, ResourceType: event.ResourceType, ResourceID: event.ResourceID, ResourceName: event.ResourceName, Method: event.Method, Path: event.Path, ClientIP: event.ClientIP, LoginIP: event.LoginIP, IngestPeerIP: event.IngestPeerIP, IPSource: event.IPSource, UserAgent: event.UserAgent, RequestID: event.RequestID, TraceID: event.TraceID, CorrelationID: event.CorrelationID, StatusCode: event.StatusCode, RiskLevel: event.RiskLevel, Detail: event.Detail, Summary: event.Summary, ChangeSummary: event.ChangeSummary}
}
func exportToResponse(job domain.ExportJob) exportResponse {
	result := exportResponse{JobID: job.JobID, Status: job.Status, DownloadURL: job.DownloadURL, ErrorCode: job.ErrorCode, ErrorMessage: job.ErrorMessage, CreatedAt: job.CreatedAt}
	if !job.CompletedAt.IsZero() {
		completed := job.CompletedAt
		result.CompletedAt = &completed
	}
	return result
}
func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}
func parseTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &parsed
}
func storagePath(root, relativePath string) (string, error) {
	if filepath.IsAbs(relativePath) {
		return "", errors.New("audit export path must be relative")
	}
	absolutePath := filepath.Join(root, filepath.FromSlash(relativePath))
	resolvedRelative, err := filepath.Rel(root, absolutePath)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return "", errors.New("audit export path escapes storage root")
	}
	return absolutePath, nil
}

func clientIP(r *http.Request) string {
	if trusted := strings.TrimSpace(requestctx.ClientIP(r.Context())); net.ParseIP(trusted) != nil {
		return net.ParseIP(trusted).String()
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil || host == "" {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
