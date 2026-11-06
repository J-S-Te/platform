// Package http 通过统一响应信封提供审计写入和只读控制台接口。
package http

import (
	"context"
	"encoding/hex"
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
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

const maxRequestBytes = 256 << 10

// exportDownloadTicketHeader 是下载导出文件时携带一次性票据的请求头。
// 刻意不支持 ?ticket= 查询参数：URL 会被浏览器历史、代理与访问日志留存，
// 等于把 bearer 能力写进日志链（与文件网关 v2 票据的取舍一致）。
const exportDownloadTicketHeader = "X-Audit-Export-Ticket"

var (
	// errExportDownloadTicketRequired 表示下载请求缺少一次性票据。
	errExportDownloadTicketRequired = httperror.New("PLATFORM_AUDIT_EXPORT_TICKET_REQUIRED", "下载审计导出文件前必须先申请一次性下载票据", nil)
	// errExportDownloadTicketInvalid 表示票据不存在、跨任务/跨租户或已被使用。
	errExportDownloadTicketInvalid = httperror.New("PLATFORM_AUDIT_EXPORT_TICKET_INVALID", "审计导出下载票据无效或已使用，请重新申请", nil)
	// errExportDownloadTicketExpired 表示票据已超过有效期。
	errExportDownloadTicketExpired = httperror.New("PLATFORM_AUDIT_EXPORT_TICKET_EXPIRED", "审计导出下载票据已过期，请重新申请", nil)
)

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
	// LoginIPSource 是登录地址的来源标记，与 ip_source（当前操作地址来源）互相独立。
	LoginIPSource string `json:"login_ip_source,omitempty"`
	// UserLoginIP 是旧协议的兼容字段：只填登录地址，缺省时按 LEGACY_PROTOCOL 处理。
	UserLoginIP   string               `json:"user_login_ip,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
	ChangeSummary []domain.FieldChange `json:"change_summary,omitempty"`
}

type batchPayload struct {
	Events []eventInputPayload `json:"events"`
}

type exportPayload struct {
	ApplicationCode string `json:"application_code,omitempty"`
	EnvironmentCode string `json:"environment_code,omitempty"`
	Action          string `json:"action,omitempty"`
	ActionCategory  string `json:"action_category,omitempty"`
	Result          string `json:"result,omitempty"`
	RiskLevel       string `json:"risk_level,omitempty"`
	Keyword         string `json:"keyword,omitempty"`
	// 与列表共用同一组精确过滤条件；导出不得比页面看到得更宽。
	EventID       string     `json:"event_id,omitempty"`
	ActorID       string     `json:"actor_id,omitempty"`
	CorrelationID string     `json:"correlation_id,omitempty"`
	ClientIP      string     `json:"client_ip,omitempty"`
	LoginIP       string     `json:"login_ip,omitempty"`
	ResourceID    string     `json:"resource_id,omitempty"`
	RequestID     string     `json:"request_id,omitempty"`
	OccurredFrom  *time.Time `json:"occurred_from,omitempty"`
	OccurredTo    *time.Time `json:"occurred_to,omitempty"`
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}
type eventResponse struct {
	EventID             string     `json:"event_id"`
	OccurredAt          time.Time  `json:"occurred_at"`
	ReceivedAt          *time.Time `json:"received_at,omitempty"`
	OperatorDisplayName string     `json:"operator_display_name"`
	// 稳定定位字段：名称会变、用户会重名，追溯必须依赖 ID 与类型。
	ActorID         string               `json:"actor_id,omitempty"`
	ActorType       string               `json:"actor_type,omitempty"`
	ReasonCode      string               `json:"reason_code,omitempty"`
	BusinessID      string               `json:"business_id,omitempty"`
	LoginIPSource   string               `json:"login_ip_source,omitempty"`
	ActionType      string               `json:"action_type"`
	ApplicationCode string               `json:"application_code"`
	ApplicationName string               `json:"application_name"`
	EnvironmentCode string               `json:"environment_code"`
	Action          string               `json:"action"`
	Result          string               `json:"result"`
	ResourceType    string               `json:"resource_type"`
	ResourceID      string               `json:"resource_id,omitempty"`
	ResourceName    string               `json:"resource_name,omitempty"`
	Method          string               `json:"method,omitempty"`
	Path            string               `json:"path,omitempty"`
	ClientIP        string               `json:"client_ip,omitempty"`
	LoginIP         string               `json:"login_ip,omitempty"`
	IngestPeerIP    string               `json:"ingest_peer_ip,omitempty"`
	IPSource        string               `json:"ip_source,omitempty"`
	UserAgent       string               `json:"user_agent,omitempty"`
	RequestID       string               `json:"request_id,omitempty"`
	TraceID         string               `json:"trace_id,omitempty"`
	CorrelationID   string               `json:"correlation_id,omitempty"`
	StatusCode      int                  `json:"status_code,omitempty"`
	RiskLevel       string               `json:"risk_level"`
	Detail          string               `json:"detail,omitempty"`
	Summary         string               `json:"summary,omitempty"`
	ChangeSummary   []domain.FieldChange `json:"change_summary"`
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

// exportDownloadTicketResponse 是签发一次性下载票据的响应。
//
// ticket 是明文 bearer 能力，只在本次响应里出现一次：不写日志、不入审计载荷、前端不持久化。
// download_url 仍指向原下载地址，但该地址现在必须携带 ticket 才能兑换文件。
type exportDownloadTicketResponse struct {
	Ticket      string    `json:"ticket"`
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
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

// payloadHashVerificationResponse 是完整性摘要重算比对的对外结果；摘要以十六进制呈现。
type payloadHashVerificationResponse struct {
	EventID      string `json:"event_id"`
	Algorithm    string `json:"algorithm"`
	Matched      bool   `json:"matched"`
	StoredHash   string `json:"stored_hash"`
	ComputedHash string `json:"computed_hash"`
}

// VerifyEventHash 以操作者提交的事件输入（与摄取时相同的字段值）重算完整性摘要并与存储行比对
// （审计 P0 · 防篡改取证，Vault /sys/audit-hash 同一模式）。路径落在 /api/v1/audit/ 下，
// 本次核验动作本身会被通用审计轨迹记录。
func (h *Handler) VerifyEventHash(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var input application.EventInput
	if !decode(w, r, &input) {
		return
	}
	// 核验口径必须与摄取一致：摄取侧先对敏感 change_summary 打码，再对规范化输入计算
	// payload_hash。候选输入走同一条打码路径，才不会把"提交了原始明文"的合法载荷误判
	// 为篡改（打码是幂等的，提交已打码值同样通过）。
	changes, err := sanitizeChangeSummary(input.Changes)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	input.Changes = changes
	verification, err := h.service.VerifyPayloadHash(r.Context(), principal.Tenant.ID, r.PathValue("event_id"), input)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计事件摘要核验完成", payloadHashVerificationResponse{
		EventID:      verification.EventID,
		Algorithm:    verification.Algorithm,
		Matched:      verification.Matched,
		StoredHash:   hex.EncodeToString(verification.StoredHash),
		ComputedHash: hex.EncodeToString(verification.ComputedHash),
	})
}

// chainVerificationResponse 是哈希链分段校验的对外结果。
type chainVerificationResponse struct {
	ApplicationCode string `json:"application_code"`
	EnvironmentCode string `json:"environment_code"`
	FromSequence    uint64 `json:"from_sequence"`
	CheckedRows     int    `json:"checked_rows"`
	LastSequence    uint64 `json:"last_sequence"`
	Verified        bool   `json:"verified"`
	MoreRemaining   bool   `json:"more_remaining"`
	BrokenSequence  uint64 `json:"broken_sequence,omitempty"`
	BrokenEventID   string `json:"broken_event_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// VerifyEventChain 校验（租户、应用、环境）维度的哈希链分段（审计 P1 · 防篡改取证）。
// 行级自证：H(chain_prev_hash, payload_hash) == chain_hash；窗口内序列连续、链指针连续；
// 窗口超上限时 more_remaining=true，可携 from_sequence 续跑。核验动作本身被通用审计轨迹记录。
func (h *Handler) VerifyEventChain(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	fromSequence, _ := strconv.ParseUint(query.Get("from_sequence"), 10, 64)
	limit, _ := strconv.ParseInt(query.Get("limit"), 10, 64)
	verification, err := h.service.VerifyEventChain(r.Context(), principal.Tenant.ID,
		strings.TrimSpace(query.Get("application")), strings.TrimSpace(query.Get("environment")),
		fromSequence, int(limit))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "哈希链校验完成", chainVerificationResponse{
		ApplicationCode: verification.ApplicationCode,
		EnvironmentCode: verification.EnvironmentCode,
		FromSequence:    verification.FromSequence,
		CheckedRows:     verification.CheckedRows,
		LastSequence:    verification.LastSequence,
		Verified:        verification.Verified,
		MoreRemaining:   verification.MoreRemaining,
		BrokenSequence:  verification.BrokenSequence,
		BrokenEventID:   verification.BrokenEventID,
		Reason:          verification.Reason,
	})
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
	job, err := h.service.CreateExportJob(r.Context(), principal.Tenant.ID, principal.User.ID, application.PageRequest{Keyword: payload.Keyword, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.EnvironmentCode, Action: payload.Action, ActionCategory: payload.ActionCategory, Result: payload.Result, RiskLevel: payload.RiskLevel, OccurredFrom: payload.OccurredFrom, OccurredTo: payload.OccurredTo, EventID: payload.EventID, ActorID: payload.ActorID, CorrelationID: payload.CorrelationID, ClientIP: payload.ClientIP, LoginIP: payload.LoginIP, ResourceID: payload.ResourceID, RequestID: payload.RequestID})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "审计导出任务已创建", exportToResponse(job))
}

// CreateExportDownloadTicket 为已完成的审计导出签发短时一次性下载票据。
//
// 签发前置条件：任务属于当前租户、状态 SUCCEEDED、产物版本仍可下载且磁盘文件存在。
// 服务端只保存票据摘要，明文票据只在本响应里出现一次；调用方不得记录日志或持久化。
func (h *Handler) CreateExportDownloadTicket(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	jobID := strings.TrimSpace(r.PathValue("job_id"))
	// 先确认产物真实存在于本地存储：任务标记 SUCCEEDED 但文件已丢失时，不签发
	// 注定无法兑换的票据。
	if _, err := h.exportArtifact(r.Context(), principal.Tenant.ID, jobID); err != nil {
		h.writeError(w, r, err)
		return
	}
	ticket, token, err := h.service.IssueExportDownloadTicket(r.Context(), principal.Tenant.ID, jobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "审计导出下载票据已签发", exportDownloadTicketResponse{Ticket: token, DownloadURL: auditExportDownloadPath(jobID), ExpiresAt: ticket.ExpiresAt.UTC()})
}

// exportArtifact 解析任务产物元数据并确认磁盘文件存在且是普通文件。
// 错误语义与既有下载保持一致：任务不存在/未完成 → ErrNotFound，产物被清理 → ErrExpired；
// 磁盘文件缺失同样按 ErrNotFound 处理（与“从未存在”一致），底层 IO 错误交给 writeError 记录。
func (h *Handler) exportArtifact(ctx context.Context, tenantID, jobID string) (domain.ExportFile, error) {
	file, err := h.service.GetExportFile(ctx, tenantID, jobID)
	if err != nil {
		return domain.ExportFile{}, err
	}
	absolutePath, err := storagePath(h.storageRoot, file.StorageRelativePath)
	if err != nil {
		return domain.ExportFile{}, err
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.ExportFile{}, application.ErrNotFound
		}
		return domain.ExportFile{}, err
	}
	if !info.Mode().IsRegular() {
		return domain.ExportFile{}, application.ErrNotFound
	}
	return file, nil
}

// DownloadExport 用一次性票据兑换并流式返回一个已完成的租户隔离导出。
// 票据原子核销成功后才会读取文件；绝不从请求接受存储路径，并在打开文件前校验路径合法性。
func (h *Handler) DownloadExport(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	jobID := strings.TrimSpace(r.PathValue("job_id"))
	ticket := strings.TrimSpace(r.Header.Get(exportDownloadTicketHeader))
	if ticket == "" {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, errExportDownloadTicketRequired)
		return
	}
	// 核销先于任何文件访问：无效、已用、过期或跨任务/跨租户的票据在这里被拒绝，
	// 服务端不会因为一次失败的兑换暴露文件是否存在。
	if err := h.service.ConsumeExportDownloadTicket(r.Context(), principal.Tenant.ID, jobID, ticket); err != nil {
		h.writeError(w, r, err)
		return
	}
	file, err := h.exportArtifact(r.Context(), principal.Tenant.ID, jobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	absolutePath, err := storagePath(h.storageRoot, file.StorageRelativePath)
	if err != nil {
		h.logger.Error("resolve audit export path", "job_id", jobID, "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
		return
	}
	opened, err := os.Open(absolutePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
			return
		}
		h.logger.Error("open audit export", "job_id", jobID, "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
		return
	}
	defer opened.Close()

	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() {
		h.logger.Error("inspect audit export", "job_id", jobID, "error", err)
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
	case errors.Is(err, application.ErrDownloadTicketExpired):
		// 票据存在但已过期：410 让前端提示“重新申请票据”，与“票据根本无效”的 403 区分。
		httpresponse.WriteError(w, r, http.StatusGone, errExportDownloadTicketExpired)
	case errors.Is(err, application.ErrDownloadTicketInvalid):
		// 不存在、跨任务/跨租户与已使用统一 403，避免向调用方区分“猜错的票据”和“已兑换的票据”。
		httpresponse.WriteError(w, r, http.StatusForbidden, errExportDownloadTicketInvalid)
	case errors.Is(err, application.ErrExpired):
		// 导出产物超过保留期被清理：410 明确告知“曾存在但不可再获取”，
		// 与从未存在的 404 区分，前端可以给出“请重新发起导出”的提示。
		httpresponse.WriteError(w, r, http.StatusGone, httperror.Gone)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	default:
		h.logger.Error("audit request failed", "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

// auditExportDownloadPath 是导出文件的受控下载地址。下载现在必须携带一次性票据，
// 但地址本身仍由服务端给出，前端不自行拼接。
func auditExportDownloadPath(jobID string) string {
	return "/api/v1/audit/export-jobs/" + url.PathEscape(jobID) + "/download"
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
	query := application.PageRequest{Page: parseInt(values.Get("page"), 1), PageSize: parseInt(values.Get("page_size"), 20), Keyword: strings.TrimSpace(values.Get("keyword")), ApplicationCode: strings.TrimSpace(filterValue(values, "application_code")), EnvironmentCode: strings.TrimSpace(filterValue(values, "environment_code")), Action: strings.TrimSpace(filterValue(values, "action")), ActionCategory: strings.TrimSpace(filterValue(values, "action_category")), Result: strings.TrimSpace(filterValue(values, "result")), RiskLevel: strings.TrimSpace(filterValue(values, "risk_level")), ClientIP: strings.TrimSpace(filterValue(values, "client_ip")), ResourceID: strings.TrimSpace(filterValue(values, "resource_id")), RequestID: strings.TrimSpace(filterValue(values, "request_id")), EventID: strings.TrimSpace(filterValue(values, "event_id")), ActorID: strings.TrimSpace(filterValue(values, "actor_id")), CorrelationID: strings.TrimSpace(filterValue(values, "correlation_id")), LoginIP: strings.TrimSpace(filterValue(values, "login_ip"))}
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
	metadata, err := copyMetadata(payload.Metadata)
	if err != nil {
		return application.EventInput{}, err
	}
	// change_summary 与 metadata 不同：它是合法审计内容，不能因为出现敏感字段就整条
	// 拒绝（否则密码重置这类事件无法留痕）。命中敏感键的值打码保留 field 名称，
	// 条数/长度超限才按校验失败拒绝（HTTP 422）。
	changes, err := sanitizeChangeSummary(payload.ChangeSummary)
	if err != nil {
		return application.EventInput{}, err
	}
	// method 与 path 可以描述被审计的子系统操作。保留上报方提供的值，仅在
	// 上报应用未提供时补充接收侧的值。
	if _, ok := metadata["method"]; !ok {
		metadata["method"] = r.Method
	}
	if _, ok := metadata["path"]; !ok {
		metadata["path"] = r.URL.Path
	}
	loginIP, clientIP, sourceFlag, legacySourceIP, loginIPSource, err := resolveEventIPs(payload, r)
	if err != nil {
		return application.EventInput{}, err
	}
	return application.EventInput{LoginIP: loginIP, ClientIP: clientIP, IngestPeerIP: deliveryPeerIP(r), IPSource: sourceFlag, LoginIPSource: loginIPSource, EventID: payload.EventID, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.EnvironmentCode, ActorType: payload.ActorType, ActorID: payload.ActorID, ActorName: payload.ActorName, SessionID: payload.SessionID, OccurredAt: payload.OccurredAt, Action: payload.Action, ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, ResourceName: payload.ResourceName, BusinessID: payload.BusinessID, RequestID: payload.RequestID, TraceID: payload.TraceID, CorrelationID: payload.CorrelationID, Result: payload.Result, ReasonCode: payload.ReasonCode, RiskLevel: payload.RiskLevel, Classification: payload.Classification, Summary: payload.Summary, Metadata: metadata, Changes: changes, SourceIP: legacySourceIP, UserAgent: r.UserAgent()}, nil
}

// resolveEventIPs 依据统一审计来源协议解析三类地址。
//
// 规则：
//   - 新协议字段（login_ip / client_ip）优先，必须是合法 IP 字面量；
//   - 只提供旧字段 user_login_ip 时按 LEGACY_PROTOCOL 处理，并只当作"登录地址"；
//   - 什么都没提供时登录/操作地址保持空，由界面显示"未采集"，绝不用投递地址冒充；
//   - legacySourceIP 仅用于兼容旧列 source_ip，保持历史读数的连续性。
func resolveEventIPs(payload eventInputPayload, r *http.Request) (loginIP, clientIP, ipSource, legacySourceIP, loginIPSource string, err error) {
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
			return "", "", "", "", "", err
		}
	}
	if payload.ClientIP != "" {
		if clientIP, err = parse("client_ip", payload.ClientIP); err != nil {
			return "", "", "", "", "", err
		}
	}
	if loginIP == "" && clientIP == "" && payload.UserLoginIP != "" {
		// 旧发布方只上报登录地址。
		if loginIP, err = parse("user_login_ip", payload.UserLoginIP); err != nil {
			return "", "", "", "", "", err
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
	// 登录地址来源独立于当前操作地址来源：只接受显式上报，缺失时留空。
	if strings.TrimSpace(payload.LoginIPSource) != "" {
		loginIPSource = normalizeAuditIPSource(payload.LoginIPSource)
	} else if loginIP != "" {
		loginIPSource = "LEGACY_PROTOCOL"
	}
	return loginIP, clientIP, ipSource, legacySourceIP, loginIPSource, nil
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

// 敏感键匹配规则（统一审计方案 §3.4）：发布端先过滤，摄取侧再防御一次。
//
// 只用"精确键 + 词缀"匹配，不做任意包含匹配——`authorization_revision` 是合法的
// 授权版本号，含 "authorization" 但不是凭据，不能被误杀。
//
// 具体规则与存储层共用 domain.IsSensitiveAuditField：平台内部适配器不经过 HTTP
// 摄取边界，两份词表必须同源，否则 private_key/otp/pin/jwt/signature 等字段仍可能
// 以明文进入只追加审计存储。
func isSensitiveMetadataKey(key string) bool {
	return domain.IsSensitiveAuditField(key)
}

// sensitiveRedaction 是敏感值在审计载荷中的固定替换串：保留字段名以便审计读者知道
// "这个字段变更过"，但不保留任何原文内容或长度特征。
const sensitiveRedaction = "***"

// maxChangeSummaryFieldRunes 限制 change_summary 的 field 名称长度。changes 列是 JSON，
// 没有列宽约束，这里只防"字段名本身被用来放大载荷"。
const maxChangeSummaryFieldRunes = 255

// sanitizeChangeSummary 在摄取边界对 change_summary 做敏感键打码与边界校验：
//   - field 命中敏感词表（与 metadata 共用）时，before/after 一律替换为 "***"，
//     仍保留 field 名称供审计追溯；
//   - before/after 内部嵌套的对象键与数组元素同样递归打码，避免
//     {"before":{"password":"x"}} 这类嵌套凭据绕过；
//   - 条数或单值长度超限返回校验错误（HTTP 422），不做静默截断。
func sanitizeChangeSummary(changes []domain.FieldChange) ([]domain.FieldChange, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	if len(changes) > application.MaxChangeSummaryEntries {
		return nil, fmt.Errorf("%w: change_summary must not exceed %d entries", application.ErrValidation, application.MaxChangeSummaryEntries)
	}
	sanitized := make([]domain.FieldChange, 0, len(changes))
	for _, change := range changes {
		// field 名称原样保留（审计定位用），仅用裁剪后的副本做敏感键判定与长度校验。
		field := strings.TrimSpace(change.Field)
		if utf8.RuneCountInString(field) > maxChangeSummaryFieldRunes {
			return nil, fmt.Errorf("%w: change_summary field exceeds %d characters", application.ErrValidation, maxChangeSummaryFieldRunes)
		}
		current := domain.FieldChange{Field: change.Field}
		if isSensitiveMetadataKey(field) {
			current.Before = sensitiveRedaction
			current.After = sensitiveRedaction
		} else {
			current.Before = redactChangeValue(change.Before)
			current.After = redactChangeValue(change.After)
		}
		sanitized = append(sanitized, current)
	}
	// 长度校验放在打码之后：超长的凭据值已经被替换成 "***"，不应因为原文过长而
	// 反过来拒绝整条事件（也避免把"值很大"作为可观测信号）。
	if err := application.ValidateChangeSummary(sanitized); err != nil {
		return nil, err
	}
	return sanitized, nil
}

// redactChangeValue 递归打码变更值内部的对象键与数组元素。
func redactChangeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSensitiveMetadataKey(key) {
				redacted[key] = sensitiveRedaction
				continue
			}
			redacted[key] = redactChangeValue(child)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, child := range typed {
			redacted[index] = redactChangeValue(child)
		}
		return redacted
	default:
		return value
	}
}

// copyMetadata 复制上报方的 metadata，并在摄取侧做敏感键防御。
// 返回错误表示载荷含凭据类字段，调用方必须拒绝整条事件而不是静默截断。
func copyMetadata(metadata map[string]any) (map[string]any, error) {
	if len(metadata) == 0 {
		return map[string]any{}, nil
	}
	copy := make(map[string]any, len(metadata)+2)
	for key, value := range metadata {
		if isSensitiveMetadataKey(key) {
			return nil, fmt.Errorf("%w: metadata contains sensitive field %q", application.ErrValidation, key)
		}
		if err := rejectSensitiveMetadataValue(value); err != nil {
			return nil, err
		}
		copy[key] = value
	}
	return copy, nil
}

// rejectSensitiveMetadataValue 递归检查 metadata 值内的对象与数组。数组分支不能省略：
// 旧实现只递归 map，{"metadata":{"items":[{"password":"x"}]}} 会整体绕过敏感键检查。
// 写法对齐文件任务的载荷检查（filetask/application/job_service.go）。
func rejectSensitiveMetadataValue(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveMetadataKey(key) {
				return fmt.Errorf("%w: metadata contains sensitive field %q", application.ErrValidation, key)
			}
			if err := rejectSensitiveMetadataValue(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectSensitiveMetadataValue(child); err != nil {
				return err
			}
		}
	}
	return nil
}
func eventToResponse(event domain.Event) eventResponse {
	response := eventResponse{
		EventID: event.EventID, OccurredAt: event.OccurredAt,
		OperatorDisplayName: event.OperatorDisplayName,
		ActorID:             event.ActorID, ActorType: event.ActorType,
		ReasonCode: event.ReasonCode, BusinessID: event.BusinessID, LoginIPSource: event.LoginIPSource,
		ActionType: event.ActionType, ApplicationCode: event.ApplicationCode, ApplicationName: event.ApplicationName,
		EnvironmentCode: event.EnvironmentCode, Action: event.Action, Result: event.Result,
		ResourceType: event.ResourceType, ResourceID: event.ResourceID, ResourceName: event.ResourceName,
		Method: event.Method, Path: event.Path, ClientIP: event.ClientIP, LoginIP: event.LoginIP,
		IngestPeerIP: event.IngestPeerIP, IPSource: event.IPSource, UserAgent: event.UserAgent,
		RequestID: event.RequestID, TraceID: event.TraceID, CorrelationID: event.CorrelationID,
		StatusCode: event.StatusCode, RiskLevel: event.RiskLevel, Detail: event.Detail,
		Summary: event.Summary, ChangeSummary: event.ChangeSummary,
	}
	if !event.ReceivedAt.IsZero() {
		received := event.ReceivedAt
		response.ReceivedAt = &received
	}
	return response
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
