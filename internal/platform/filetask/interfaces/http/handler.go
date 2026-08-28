// Package filetaskhttp adapts file storage and async task use cases to the platform HTTP envelope.
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
)

const (
	maxJSONRequestBytes   = 320 * 1024
	maxUploadRequestBytes = 21 << 20 // Default policy (20 MiB) plus multipart overhead.
)

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

// Handler provides tenant-scoped, authenticated operations. Route registration and permission
// middleware remain bootstrap responsibilities; download authorization is additionally enforced
// in the application service so an owner cannot be bypassed by a route misconfiguration.
type Handler struct {
	files  fileService
	jobs   jobService
	logger *slog.Logger
}

// NewHandler validates module dependencies.
func NewHandler(files fileService, jobs jobService, logger *slog.Logger) (*Handler, error) {
	if files == nil || jobs == nil || logger == nil {
		return nil, errors.New("filetask HTTP handler dependencies must not be nil")
	}
	return &Handler{files: files, jobs: jobs, logger: logger}, nil
}

// NewJobHandler 创建只承载平台异步任务的 HTTP 适配器。File Gateway 拆为独立进程后，
// 平台 API 不再构造本地文件存储实现，也不能借该构造函数重新暴露旧上传路径。
func NewJobHandler(jobs jobService, logger *slog.Logger) (*Handler, error) {
	if jobs == nil || logger == nil {
		return nil, errors.New("async job HTTP handler dependencies must not be nil")
	}
	return &Handler{jobs: jobs, logger: logger}, nil
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
	// Before is a policy-supplied UTC cutoff. The current schema has no file_object.expires_at;
	// callers must not treat it as a per-file expiry setting.
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

// Upload accepts one multipart file and persists it under the configured local storage root.
// Expected fields: application_id, classification (optional), and file.
func (handler *Handler) Upload(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

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
	fileContent, fileHeader, err := request.FormFile("file")
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	defer fileContent.Close()

	file, err := handler.files.Upload(request.Context(), application.UploadInput{
		TenantID:          principal.Tenant.ID,
		ApplicationID:     applicationID,
		OwnerUserID:       principal.User.ID,
		OriginalName:      fileHeader.Filename,
		DeclaredMediaType: fileHeader.Header.Get("Content-Type"),
		Classification:    classification,
		RequestID:         request.Header.Get("X-Request-ID"),
		Content:           fileContent,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "文件上传成功", fileToResponse(file))
}

// Download verifies tenant and owner/permission access before the binary is opened. It never
// returns the storage path, checksum, or a raw OS error to the caller.
func (handler *Handler) Download(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	fileID := strings.TrimSpace(request.PathValue("file_id"))
	stored, stream, err := handler.files.OpenDownload(request.Context(), application.DownloadAccess{
		TenantID:        principal.Tenant.ID,
		UserID:          principal.User.ID,
		PermissionCodes: principal.PermissionCodes,
	}, fileID)
	if err != nil {
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
		handler.logger.Warn("file download stream interrupted", "file_id", stored.File.ID, "error", err)
	}
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
	binding, err := handler.files.BindResource(request.Context(), application.BindingInput{
		TenantID: principal.Tenant.ID, ApplicationID: payload.ApplicationID, FileID: request.PathValue("file_id"),
		ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, BindingType: payload.BindingType,
		DisplayName: payload.DisplayName, SortOrder: payload.SortOrder, OperatorUserID: principal.User.ID,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "文件绑定成功", binding)
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

// CreateJob queues an application-owned job. Only a registered worker may interpret its payload.
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

// ListJobs returns bounded operational fields. Payload is intentionally omitted because it can
// carry business data and workers, rather than operations users, own its interpretation.
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

// CancelJob stops a queued or terminal task. Running-task cooperative cancellation needs the
// future cancellation-state migration described in the module document.
func (handler *Handler) CancelJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已取消", func(ctx context.Context, tenantID, jobID string) (any, error) {
		return nil, handler.jobs.Cancel(ctx, tenantID, jobID)
	})
}

// RetryJob returns a FAILED or DEAD job to the queue with attempts reset.
func (handler *Handler) RetryJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已重试", func(ctx context.Context, tenantID, jobID string) (any, error) {
		return nil, handler.jobs.Retry(ctx, tenantID, jobID)
	})
}

// RerunJob creates a new job from a terminal source job and therefore preserves the source
// record. The current table does not retain a durable rerun-parent link.
func (handler *Handler) RerunJob(writer http.ResponseWriter, request *http.Request) {
	handler.mutateJob(writer, request, "异步任务已重新创建", func(ctx context.Context, tenantID, jobID string) (any, error) {
		job, err := handler.jobs.Rerun(ctx, tenantID, jobID)
		if err != nil {
			return nil, err
		}
		return jobToResponse(job), nil
	})
}

// CleanupFiles executes one bounded cleanup pass. It is intended for an administrator-triggered
// operation or a future scheduled worker, not for arbitrary end-user requests.
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
