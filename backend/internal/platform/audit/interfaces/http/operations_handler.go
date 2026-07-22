// Package http provides tenant-scoped audit operations handlers. Route registration and permission
// middleware remain in the transport layer so this module has no hidden authorization bypass.
package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

// OperationsHandler exposes retention, receipt, and dead-letter operations. It is intentionally
// separate from Handler so existing ingestion and query routes keep their constructor contract.
type OperationsHandler struct {
	retention   *application.RetentionService
	deadLetters *application.DeadLetterService
	receipts    *application.IngestionReceiptService
	logger      *slog.Logger
}

func NewOperationsHandler(retention *application.RetentionService, deadLetters *application.DeadLetterService, receipts *application.IngestionReceiptService, logger *slog.Logger) (*OperationsHandler, error) {
	if retention == nil || deadLetters == nil || receipts == nil || logger == nil {
		return nil, errors.New("audit operations HTTP handler dependencies must not be nil")
	}
	return &OperationsHandler{retention: retention, deadLetters: deadLetters, receipts: receipts, logger: logger}, nil
}

type retentionTaskPayload struct {
	ApplicationID string    `json:"application_id"`
	Mode          string    `json:"mode"`
	ArchiveID     string    `json:"archive_id,omitempty"`
	CutoffAt      time.Time `json:"cutoff_at"`
}

type deadLetterReplayBatchPayload struct {
	DeadLetterIDs []string `json:"dead_letter_ids"`
}

type retentionTaskResponse struct {
	TaskID         string     `json:"task_id"`
	ApplicationID  string     `json:"application_id"`
	RequestedBy    string     `json:"requested_by"`
	Mode           string     `json:"mode"`
	Status         string     `json:"status"`
	ArchiveID      string     `json:"archive_id,omitempty"`
	CutoffAt       time.Time  `json:"cutoff_at"`
	CandidateCount uint64     `json:"candidate_count"`
	ProcessedCount uint64     `json:"processed_count"`
	FailureCode    string     `json:"failure_code,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

type ingestionReceiptResponse struct {
	ID              uint64    `json:"id"`
	ApplicationCode string    `json:"application_code"`
	ApplicationName string    `json:"application_name,omitempty"`
	EnvironmentCode string    `json:"environment_code"`
	ClientID        string    `json:"client_id,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	TraceID         string    `json:"trace_id,omitempty"`
	CorrelationID   string    `json:"correlation_id,omitempty"`
	EventCount      uint      `json:"event_count"`
	AcceptedCount   uint      `json:"accepted_count"`
	DuplicateCount  uint      `json:"duplicate_count"`
	Status          string    `json:"status"`
	SourceIP        string    `json:"source_ip,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
}

type deadLetterResponse struct {
	DeadLetterID    string     `json:"dead_letter_id"`
	ApplicationCode string     `json:"application_code"`
	EnvironmentCode string     `json:"environment_code"`
	EventID         string     `json:"event_id"`
	Status          string     `json:"status"`
	LastErrorCode   string     `json:"last_error_code,omitempty"`
	Attempts        uint       `json:"attempts"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ReplayedAt      *time.Time `json:"replayed_at,omitempty"`
}

type deadLetterStatusResponse struct {
	ApplicationCode string     `json:"application_code,omitempty"`
	Pending         uint64     `json:"pending"`
	Replayed        uint64     `json:"replayed"`
	Ignored         uint64     `json:"ignored"`
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"`
}

type deadLetterReplayResponse struct {
	DeadLetterID  string     `json:"dead_letter_id"`
	EventID       string     `json:"event_id,omitempty"`
	Status        string     `json:"status"`
	ReceiptStatus string     `json:"receipt_status,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	ReplayedAt    *time.Time `json:"replayed_at,omitempty"`
}

// CreateRetentionTask accepts only an async archive or a manifest-bound purge request. It never
// exposes a direct audit-event deletion endpoint.
func (h *OperationsHandler) CreateRetentionTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	var payload retentionTaskPayload
	if !decode(w, r, &payload) {
		return
	}
	task, err := h.retention.Request(r.Context(), application.RetentionTaskInput{
		TenantID:      principal.Tenant.ID,
		ApplicationID: strings.TrimSpace(payload.ApplicationID),
		RequestedBy:   principal.User.ID,
		Mode:          strings.TrimSpace(payload.Mode),
		ArchiveID:     strings.TrimSpace(payload.ArchiveID),
		CutoffAt:      payload.CutoffAt,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "审计保留任务已创建", retentionTaskToResponse(task))
}

func (h *OperationsHandler) ListRetentionTasks(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	result, err := h.retention.List(r.Context(), principal.Tenant.ID, application.RetentionTaskPageRequest{
		Page:          parseInt(query.Get("page"), 1),
		PageSize:      parseInt(query.Get("page_size"), 20),
		ApplicationID: strings.TrimSpace(query.Get("filter[application_id]")),
		Mode:          strings.TrimSpace(query.Get("filter[mode]")),
		Status:        strings.TrimSpace(query.Get("filter[status]")),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]retentionTaskResponse, 0, len(result.Items))
	for _, task := range result.Items {
		items = append(items, retentionTaskToResponse(task))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计保留任务查询成功", pageResponse[retentionTaskResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

func (h *OperationsHandler) ListIngestionReceipts(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	result, err := h.receipts.List(r.Context(), principal.Tenant.ID, application.IngestionReceiptPageRequest{
		Page:            parseInt(query.Get("page"), 1),
		PageSize:        parseInt(query.Get("page_size"), 20),
		ApplicationCode: strings.TrimSpace(query.Get("filter[application_code]")),
		EnvironmentCode: strings.TrimSpace(query.Get("filter[environment_code]")),
		Status:          strings.TrimSpace(query.Get("filter[status]")),
		RequestID:       strings.TrimSpace(query.Get("filter[request_id]")),
		CorrelationID:   strings.TrimSpace(query.Get("filter[correlation_id]")),
		ReceivedFrom:    parseTime(query.Get("filter[received_from]")),
		ReceivedTo:      parseTime(query.Get("filter[received_to]")),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]ingestionReceiptResponse, 0, len(result.Items))
	for _, receipt := range result.Items {
		items = append(items, receiptToResponse(receipt))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计接收回执查询成功", pageResponse[ingestionReceiptResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

func (h *OperationsHandler) ListDeadLetters(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	result, err := h.deadLetters.List(r.Context(), principal.Tenant.ID, application.DeadLetterPageRequest{
		Page:            parseInt(query.Get("page"), 1),
		PageSize:        parseInt(query.Get("page_size"), 20),
		ApplicationCode: strings.TrimSpace(query.Get("filter[application_code]")),
		Status:          strings.TrimSpace(query.Get("filter[status]")),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]deadLetterResponse, 0, len(result.Items))
	for _, letter := range result.Items {
		items = append(items, deadLetterToResponse(letter))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计死信查询成功", pageResponse[deadLetterResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// GetDeadLetter returns controlled operational metadata for one tenant-scoped dead letter. The
// DTO intentionally excludes its stored payload and raw last-error message.
func (h *OperationsHandler) GetDeadLetter(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	letter, err := h.deadLetters.Get(r.Context(), principal.Tenant.ID, r.PathValue("dead_letter_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计死信查询成功", deadLetterToResponse(letter))
}

func (h *OperationsHandler) GetDeadLetterStatus(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	status, err := h.deadLetters.Status(r.Context(), principal.Tenant.ID, strings.TrimSpace(r.URL.Query().Get("filter[application_code]")))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计死信状态查询成功", deadLetterStatusResponse{ApplicationCode: status.ApplicationCode, Pending: status.Pending, Replayed: status.Replayed, Ignored: status.Ignored, OldestPendingAt: status.OldestPendingAt})
}

func (h *OperationsHandler) ReplayDeadLetter(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	result, err := h.deadLetters.ReplayBatch(r.Context(), principal.Tenant.ID, []string{r.PathValue("dead_letter_id")}, principal.User.ID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计死信重放已完成", replayResultToResponse(result[0]))
}

func (h *OperationsHandler) ReplayDeadLetters(w http.ResponseWriter, r *http.Request) {
	principal, ok := operationPrincipal(w, r)
	if !ok {
		return
	}
	var payload deadLetterReplayBatchPayload
	if !decode(w, r, &payload) {
		return
	}
	results, err := h.deadLetters.ReplayBatch(r.Context(), principal.Tenant.ID, payload.DeadLetterIDs, principal.User.ID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]deadLetterReplayResponse, 0, len(results))
	for _, result := range results {
		items = append(items, replayResultToResponse(result))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计死信批量重放已完成", items)
}

func operationPrincipal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return principal, ok
}

func (h *OperationsHandler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	default:
		h.logger.Error("audit operation request failed", "error", err)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

func retentionTaskToResponse(task domain.RetentionTask) retentionTaskResponse {
	response := retentionTaskResponse{TaskID: task.TaskID, ApplicationID: task.ApplicationID, RequestedBy: task.RequestedBy, Mode: task.Mode, Status: task.Status, ArchiveID: task.ArchiveID, CutoffAt: task.CutoffAt, CandidateCount: task.CandidateCount, ProcessedCount: task.ProcessedCount, FailureCode: task.FailureCode, CreatedAt: task.CreatedAt}
	if !task.StartedAt.IsZero() {
		value := task.StartedAt
		response.StartedAt = &value
	}
	if !task.CompletedAt.IsZero() {
		value := task.CompletedAt
		response.CompletedAt = &value
	}
	return response
}

func receiptToResponse(receipt domain.IngestionReceipt) ingestionReceiptResponse {
	return ingestionReceiptResponse{ID: receipt.ID, ApplicationCode: receipt.ApplicationCode, ApplicationName: receipt.ApplicationName, EnvironmentCode: receipt.EnvironmentCode, ClientID: receipt.ClientID, RequestID: receipt.RequestID, TraceID: receipt.TraceID, CorrelationID: receipt.CorrelationID, EventCount: receipt.EventCount, AcceptedCount: receipt.AcceptedCount, DuplicateCount: receipt.DuplicateCount, Status: receipt.Status, SourceIP: receipt.SourceIP, ReceivedAt: receipt.ReceivedAt}
}

func deadLetterToResponse(letter domain.DeadLetter) deadLetterResponse {
	response := deadLetterResponse{DeadLetterID: letter.DeadLetterID, ApplicationCode: letter.ApplicationCode, EnvironmentCode: letter.EnvironmentCode, EventID: letter.EventID, Status: letter.Status, LastErrorCode: string(letter.LastErrorCode), Attempts: letter.Attempts, CreatedAt: letter.CreatedAt, UpdatedAt: letter.UpdatedAt}
	if !letter.ReplayedAt.IsZero() {
		value := letter.ReplayedAt
		response.ReplayedAt = &value
	}
	return response
}

func replayResultToResponse(result domain.DeadLetterReplayResult) deadLetterReplayResponse {
	response := deadLetterReplayResponse{DeadLetterID: result.DeadLetterID, EventID: result.EventID, Status: result.Status, ReceiptStatus: result.ReceiptStatus, ErrorCode: result.ErrorCode}
	if !result.ReplayedAt.IsZero() {
		value := result.ReplayedAt
		response.ReplayedAt = &value
	}
	return response
}
