// Package filetaskhttp 把文件存储与异步任务用例适配到平台 HTTP 响应封装。
package filetaskhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"gorm.io/gorm"
)

const (
	maxJSONRequestBytes   = 320 * 1024
	maxUploadRequestBytes = 21 << 20 // 默认策略 20MiB 加 multipart 编码开销。
	// v1MaxConcurrentFileOps 是 v1 上传/下载的每实例并发上限（AUD-2026-025）。v1 路径经
	// ParseMultipartForm 缓冲整个请求体、服务层再整段读入校验（最坏约两份 20MiB 缓冲），
	// 与 v2 会话路径按申报大小流式落盘不同，故 v2 不加该信号量。上限取常量以约束单实例
	// 最坏内存占用；限流在鉴权之后执行，未认证请求不占用名额。
	v1MaxConcurrentFileOps = 4
)

// fileGatewayBusy 是 v1 文件操作并发超限时的稳定错误（503），沿用平台统一错误信封。
var fileGatewayBusy = httperror.New("FILE_GATEWAY_BUSY", "文件网关繁忙，请稍后重试", nil)

type fileService interface {
	Upload(context.Context, application.UploadInput) (domain.File, error)
	OpenDownload(context.Context, application.DownloadAccess, string) (domain.StoredFile, io.ReadSeekCloser, error)
	BindResource(context.Context, application.BindingInput) (domain.FileBinding, error)
	UnbindResource(context.Context, string, string, string, string) error
	CleanupUnboundExpired(context.Context, string, time.Time, int) (domain.CleanupResult, error)
	ReconcileStaleUploads(context.Context, string, time.Time, int) (domain.ReconcileResult, error)
}

type jobService interface {
	Enqueue(context.Context, application.JobCreateInput) (domain.Job, error)
	List(context.Context, string, domain.PageRequest) (domain.PageResult[domain.Job], error)
	Cancel(context.Context, string, string) error
	Retry(context.Context, string, string) error
	Rerun(context.Context, string, string) (domain.Job, error)
}

// Handler 提供租户隔离的已认证操作。路由注册与权限中间件仍由装配根负责；
// 下载授权在应用服务中再加一道，避免路由配置错误绕过属主校验。
type Handler struct {
	files   fileService
	jobs    jobService
	logger  *slog.Logger
	auditDB *gorm.DB // 为空仅出现在 NewJobHandler 构造的只承载异步任务的 Handler，不挂载 v1 文件路由。
	fileOps chan struct{}
}

// NewHandler 校验模块依赖。
func NewHandler(files fileService, jobs jobService, logger *slog.Logger, auditDB *gorm.DB) (*Handler, error) {
	if files == nil || jobs == nil || logger == nil {
		return nil, errors.New("filetask HTTP handler dependencies must not be nil")
	}
	return &Handler{files: files, jobs: jobs, logger: logger, auditDB: auditDB, fileOps: make(chan struct{}, v1MaxConcurrentFileOps)}, nil
}

// NewJobHandler 创建只承载平台异步任务的 HTTP 适配器。File Gateway 拆为独立进程后，
// 平台 API 不再构造本地文件存储实现，也不能借该构造函数重新暴露旧上传路径。
func NewJobHandler(jobs jobService, logger *slog.Logger) (*Handler, error) {
	if jobs == nil || logger == nil {
		return nil, errors.New("async job HTTP handler dependencies must not be nil")
	}
	return &Handler{jobs: jobs, logger: logger, fileOps: make(chan struct{}, v1MaxConcurrentFileOps)}, nil
}

type uploadResponse struct {
	FileID         string `json:"file_id"`
	OriginalName   string `json:"original_name"`
	MediaType      string `json:"media_type"`
	Classification string `json:"classification"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}

type jobPayload struct {
	ApplicationID  string          `json:"application_id"`
	JobType        string          `json:"job_type"`
	AggregateType  string          `json:"aggregate_type"`
	AggregateID    string          `json:"aggregate_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	BusinessRef    string          `json:"business_ref"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int             `json:"priority"`
	MaxAttempts    uint            `json:"max_attempts"`
	AvailableAt    *time.Time      `json:"available_at"`
}

type cleanupPayload struct {
	// Before 是策略层提供的 UTC 截止点。当前模式没有 file_object.expires_at；
	// 调用方不得把它当作逐文件过期设置。
	Before   time.Time `json:"before"`
	MaxFiles int       `json:"max_files"`
}

type bindingPayload struct {
	ApplicationID string `json:"application_id"`
	ResourceType  string `json:"resource_type"`
	ResourceID    string `json:"resource_id"`
	BindingType   string `json:"binding_type"`
	DisplayName   string `json:"display_name"`
	SortOrder     int    `json:"sort_order"`
}

type reconcilePayload struct {
	Before time.Time `json:"before"`
	Limit  int       `json:"limit"`
}

// Upload 接收一个 multipart 文件并持久化到配置的本地存储根下。
// 期望字段：application_id、classification（可选）和 file。
func (handler *Handler) Upload(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if !handler.limitConcurrentFileOps(writer, request) {
		return
	}
	defer handler.releaseFileOp()

	request.Body = http.MaxBytesReader(writer, request.Body, maxUploadRequestBytes)
	if err := request.ParseMultipartForm(maxUploadRequestBytes); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	applicationID := strings.TrimSpace(request.FormValue("application_id"))
	if applicationID == "" || applicationID != principal.Account.ID {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	classification := strings.TrimSpace(request.FormValue("classification"))
	purpose := strings.TrimSpace(request.FormValue("purpose"))
	namespace := ""
	policyVersion, retentionClass := "", ""
	var retentionUntil *time.Time
	if purpose != "" {
		selected, exists := uploadPolicies[purpose]
		if !exists || !validPurposeApplication(principal.Account.Code, selected.Namespace) {
			httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
			return
		}
		namespace, purpose = selected.Namespace, selected.Purpose
		policyVersion, retentionClass = v2PolicyVersion, "LONG_TERM"
		if selected.Temporary {
			value := time.Now().UTC().Add(24 * time.Hour)
			retentionUntil, retentionClass = &value, "TEMPORARY"
		}
	}
	fileContent, fileHeader, err := request.FormFile("file")
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	defer fileContent.Close()

	file, err := handler.files.Upload(request.Context(), application.UploadInput{
		TenantID:              principal.Tenant.ID,
		ApplicationID:         applicationID,
		OwnerUserID:           principal.User.ID,
		Namespace:             namespace,
		Purpose:               purpose,
		PolicyVersion:         policyVersion,
		RetentionClass:        retentionClass,
		RetentionUntil:        retentionUntil,
		AuthenticatedClientID: principal.SessionID,
		OriginalName:          fileHeader.Filename,
		DeclaredMediaType:     fileHeader.Header.Get("Content-Type"),
		Classification:        classification,
		RequestID:             request.Header.Get("X-Request-ID"),
		Content:               fileContent,
	})
	if err != nil {
		// v1 上传失败同样留访问审计（AUD-2026-022），与 v2 会话路径的 UPLOAD_COMPLETED/FAILED 对齐。
		handler.auditFileAccess(request.Context(), principal.Tenant.ID, applicationID, "", principal.User.ID, principal.SessionID, "UPLOAD_COMPLETED", "FAILED", request.Header.Get("X-Request-ID"))
		handler.writeError(writer, request, err)
		return
	}
	handler.auditFileAccess(request.Context(), principal.Tenant.ID, applicationID, file.ID, principal.User.ID, principal.SessionID, "UPLOAD_COMPLETED", "SUCCESS", request.Header.Get("X-Request-ID"))
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "文件上传成功", fileToResponse(file))
}

// Download 在打开二进制前校验租户与属主/权限访问。绝不向调用方返回存储路径、
// 校验和或原始 OS 错误。
func (handler *Handler) Download(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if !handler.limitConcurrentFileOps(writer, request) {
		return
	}
	defer handler.releaseFileOp()

	fileID := strings.TrimSpace(request.PathValue("file_id"))
	stored, stream, err := handler.files.OpenDownload(request.Context(), application.DownloadAccess{
		TenantID:        principal.Tenant.ID,
		UserID:          principal.User.ID,
		ApplicationID:   principal.Account.ID,
		PermissionCodes: principal.PermissionCodes,
	}, fileID)
	if err != nil {
		handler.auditFileAccess(request.Context(), principal.Tenant.ID, principal.Account.ID, fileID, principal.User.ID, principal.SessionID, "DOWNLOAD_COMPLETED", "FAILED", request.Header.Get("X-Request-ID"))
		handler.writeError(writer, request, err)
		return
	}
	defer stream.Close()

	writer.Header().Set("Content-Type", stored.Version.MediaType)
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": stored.Version.OriginalName}))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "private, no-store")
	if stored.Version.SizeBytes > 0 {
		writer.Header().Set("Content-Length", strconv.FormatUint(stored.Version.SizeBytes, 10))
	}
	if _, err := io.Copy(writer, stream); err != nil {
		handler.auditFileAccess(request.Context(), principal.Tenant.ID, principal.Account.ID, stored.File.ID, principal.User.ID, principal.SessionID, "DOWNLOAD_COMPLETED", "FAILED", request.Header.Get("X-Request-ID"))
		handler.logger.Warn("file download stream interrupted", "file_id", stored.File.ID, "error", err)
		return
	}
	handler.auditFileAccess(request.Context(), principal.Tenant.ID, principal.Account.ID, stored.File.ID, principal.User.ID, principal.SessionID, "DOWNLOAD_COMPLETED", "SUCCESS", request.Header.Get("X-Request-ID"))
}

// BindFile 将 READY 文件绑定到一个业务资源；路由权限负责限制谁能操作绑定，服务层继续
// 校验文件与 application_id 的真实归属，不能依赖请求字段自行声明。
func (handler *Handler) BindFile(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload bindingPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if strings.TrimSpace(payload.ApplicationID) == "" || payload.ApplicationID != principal.Account.ID {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	// 机器客户端没有平台用户主体。使用已认证的应用 ID 作为非敏感审计操作者，
	// 与 v2 上传会话绑定路径保持一致，不信任浏览器提供的值。
	operator := bindingOperator(principal)
	binding, err := handler.files.BindResource(request.Context(), application.BindingInput{
		TenantID: principal.Tenant.ID, ApplicationID: payload.ApplicationID, FileID: request.PathValue("file_id"),
		ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, BindingType: payload.BindingType,
		DisplayName: payload.DisplayName, SortOrder: payload.SortOrder, OperatorUserID: operator,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "文件绑定成功", binding)
}

func bindingOperator(principal authctx.Principal) string {
	if userID := strings.TrimSpace(principal.User.ID); userID != "" {
		return userID
	}
	return strings.TrimSpace(principal.Account.ID)
}

// UnbindFile 停用指定资源绑定，保留数据库记录用于后续审计与排障。
func (handler *Handler) UnbindFile(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if err := handler.files.UnbindResource(request.Context(), principal.Tenant.ID, principal.Account.ID, request.PathValue("file_id"), request.PathValue("binding_id")); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "文件绑定已停用", nil)
}

// CreateJob 排队一个应用自有任务。只有注册的 worker 才能解析其 payload。
func (handler *Handler) CreateJob(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload jobPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	headerIdempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if headerIdempotencyKey != "" && strings.TrimSpace(payload.IdempotencyKey) != "" && headerIdempotencyKey != strings.TrimSpace(payload.IdempotencyKey) {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	job, err := handler.jobs.Enqueue(request.Context(), application.JobCreateInput{
		TenantID: principal.Tenant.ID, ApplicationID: payload.ApplicationID, JobType: payload.JobType,
		AggregateType: payload.AggregateType, AggregateID: payload.AggregateID, Payload: payload.Payload,
		IdempotencyKey: firstNonEmpty(headerIdempotencyKey, payload.IdempotencyKey),
		RequestID:      requestctx.RequestID(request.Context()), TraceID: requestctx.TraceID(request.Context()),
		CorrelationID: requestctx.CorrelationID(request.Context()), BusinessRef: payload.BusinessRef,
		Priority: payload.Priority, MaxAttempts: payload.MaxAttempts, AvailableAt: payload.AvailableAt,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "异步任务已创建", jobToResponse(job))
}

// ListJobs 返回有界的运营字段。有意省略 payload：它可能携带业务数据，且解释权
// 属于 worker 而非运营用户。
func (handler *Handler) ListJobs(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	pageSize, _ := strconv.Atoi(query.Get("page_size"))
	result, err := handler.jobs.List(request.Context(), principal.Tenant.ID, domain.PageRequest{
		Page: page, PageSize: pageSize, Status: query.Get("status"), JobType: query.Get("job_type"),
		ApplicationID: query.Get("application_id"), Query: query.Get("query"),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]jobResponse, 0, len(result.Items))
	for _, job := range result.Items {
		items = append(items, jobToResponse(job))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "异步任务查询成功", map[string]any{
		"items": items, "page": result.Page, "page_size": result.PageSize, "total": result.Total,
	})
}

// CancelJob 停止排队中或已终结的任务。运行中任务的协作取消需要模块文档所述的
// 取消状态迁移（尚未落地）。
func (handler *Handler) CancelJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已取消", func(ctx context.Context, tenantID, jobID string) (any, error) {
		return nil, handler.jobs.Cancel(ctx, tenantID, jobID)
	})
}

// RetryJob 把 FAILED 或 DEAD 任务放回队列并重置尝试次数。
func (handler *Handler) RetryJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已重试", func(ctx context.Context, tenantID, jobID string) (any, error) {
		return nil, handler.jobs.Retry(ctx, tenantID, jobID)
	})
}

// RerunJob 从已终结的源任务创建新任务，因此保留源记录。当前表没有持久的
// rerun 父子链接。
func (handler *Handler) RerunJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已重新创建", func(ctx context.Context, tenantID, jobID string) (any, error) {
		job, err := handler.jobs.Rerun(ctx, tenantID, jobID)
		if err != nil {
			return nil, err
		}
		return jobToResponse(job), nil
	})
}

// CleanupFiles 执行一次有界清理。面向管理员触发的操作或未来的定时 worker，
// 不面向任意终端用户请求。
func (handler *Handler) CleanupFiles(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload cleanupPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.files.CleanupUnboundExpired(request.Context(), principal.Tenant.ID, payload.Before, payload.MaxFiles)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "文件清理任务执行完成", result)
}

// ReconcileFiles 对账并恢复因进程崩溃停留在上传中间态的文件。
func (handler *Handler) ReconcileFiles(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload reconcilePayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.files.ReconcileStaleUploads(request.Context(), principal.Tenant.ID, payload.Before, payload.Limit)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "文件状态对账完成", result)
}

func (handler *Handler) mutateJob(writer http.ResponseWriter, request *http.Request, message string, action func(context.Context, string, string) (any, error)) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	jobID := strings.TrimSpace(request.PathValue("job_id"))
	if jobID == "" {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	data, err := action(request.Context(), principal.Tenant.ID, jobID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, message, data)
}

func (handler *Handler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

// limitConcurrentFileOps 为 v1 上传/下载获取每实例并发名额（AUD-2026-025，见
// v1MaxConcurrentFileOps 注释）。名额已满时写出 503 并返回 false，调用方直接返回。
func (handler *Handler) limitConcurrentFileOps(writer http.ResponseWriter, request *http.Request) bool {
	select {
	case handler.fileOps <- struct{}{}:
		return true
	default:
		handler.logger.Warn("file gateway v1 concurrency limit reached", "path", request.URL.Path)
		httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, fileGatewayBusy)
		return false
	}
}

func (handler *Handler) releaseFileOp() { <-handler.fileOps }

// auditFileAccess 为仍在路由表中的 v1 上传/下载补齐网关访问审计（AUD-2026-022），复用
// v2 的 accessAudit 写入路径与事件类型（UPLOAD_COMPLETED/DOWNLOAD_COMPLETED）。写入
// 失败仅记结构化日志，不阻断主流程。
func (handler *Handler) auditFileAccess(ctx context.Context, tenantID, applicationID, fileID, actorUserID, authenticatedClientID, action, result, requestID string) {
	if handler.auditDB == nil {
		// 只承载异步任务的 Handler（NewJobHandler）不挂载 v1 文件路由，无审计库可写。
		return
	}
	writeFileAccessAudit(ctx, handler.auditDB, handler.logger, accessAudit{
		TenantID: tenantID, ApplicationID: applicationID, FileID: fileID,
		ActorUserID: optionalString(actorUserID), AuthenticatedClientID: authenticatedClientID,
		Action: action, Result: result, RequestID: optionalString(requestID), CreatedAt: time.Now().UTC(),
	})
}

func (handler *Handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation), errors.Is(err, application.ErrPayloadUnsafe):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrForbidden):
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrStorage):
		handler.logger.Error("filetask storage request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	default:
		handler.logger.Error("filetask request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxJSONRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

type jobResponse struct {
	JobID            string `json:"job_id"`
	ParentJobID      string `json:"parent_job_id,omitempty"`
	ApplicationID    string `json:"application_id,omitempty"`
	JobType          string `json:"job_type"`
	AggregateType    string `json:"aggregate_type,omitempty"`
	AggregateID      string `json:"aggregate_id,omitempty"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	TraceID          string `json:"trace_id,omitempty"`
	CorrelationID    string `json:"correlation_id,omitempty"`
	BusinessRef      string `json:"business_ref,omitempty"`
	Status           string `json:"status"`
	Priority         int    `json:"priority"`
	Attempts         uint   `json:"attempts"`
	RetryCount       uint   `json:"retry_count"`
	MaxAttempts      uint   `json:"max_attempts"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
	ResultFileID     string `json:"result_file_id,omitempty"`
	AvailableAt      string `json:"available_at"`
	CreatedAt        string `json:"created_at"`
	CompletedAt      string `json:"completed_at,omitempty"`
	LastAttemptAt    string `json:"last_attempt_at,omitempty"`
	LastSucceededAt  string `json:"last_succeeded_at,omitempty"`
}

func fileToResponse(file domain.File) uploadResponse {
	return uploadResponse{FileID: file.ID, OriginalName: file.OriginalName, MediaType: file.MediaType, Classification: file.Classification, Status: file.Status, CreatedAt: file.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func jobToResponse(job domain.Job) jobResponse {
	response := jobResponse{JobID: job.PublicID, ParentJobID: job.ParentPublicID, ApplicationID: job.ApplicationID, JobType: job.JobType, AggregateType: job.AggregateType, AggregateID: job.AggregateID, IdempotencyKey: job.IdempotencyKey, RequestID: job.RequestID, TraceID: job.TraceID, CorrelationID: job.CorrelationID, BusinessRef: job.BusinessRef, Status: job.Status, Priority: job.Priority, Attempts: job.Attempts, RetryCount: job.RetryCount, MaxAttempts: job.MaxAttempts, LastErrorCode: job.LastErrorCode, LastErrorMessage: job.LastErrorMessage, ResultFileID: job.ResultFileID, AvailableAt: job.AvailableAt.UTC().Format(time.RFC3339Nano), CreatedAt: job.CreatedAt.UTC().Format(time.RFC3339Nano)}
	if job.CompletedAt != nil {
		response.CompletedAt = job.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	if job.LastAttemptAt != nil {
		response.LastAttemptAt = job.LastAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	if job.LastSucceededAt != nil {
		response.LastSucceededAt = job.LastSucceededAt.UTC().Format(time.RFC3339Nano)
	}
	return response
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
