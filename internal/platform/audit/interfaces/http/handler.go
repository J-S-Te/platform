// Package http exposes audit ingestion and read-only console APIs through the standard envelope.
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

type Handler struct {
	service     *application.Service
	logger      *slog.Logger
	storageRoot string
}

func NewHandler(service *application.Service, logger *slog.Logger, storageRoot string) (*Handler, error) {
	if service == nil || logger == nil || strings.TrimSpace(storageRoot) == "" {
		return nil, errors.New("audit HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger, storageRoot: filepath.Clean(storageRoot)}, nil
}

type eventInputPayload struct {
	EventID         string               `json:"event_id"`
	ApplicationCode string               `json:"application_code"`
	EnvironmentCode string               `json:"environment_code"`
	ActorType       string               `json:"actor_type,omitempty"`
	ActorID         string               `json:"actor_id,omitempty"`
	ActorName       string               `json:"actor_name,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
	OccurredAt      time.Time            `json:"occurred_at"`
	Action          string               `json:"action"`
	ResourceType    string               `json:"resource_type"`
	ResourceID      string               `json:"resource_id,omitempty"`
	ResourceName    string               `json:"resource_name,omitempty"`
	BusinessID      string               `json:"business_id,omitempty"`
	RequestID       string               `json:"request_id,omitempty"`
	TraceID         string               `json:"trace_id,omitempty"`
	CorrelationID   string               `json:"correlation_id,omitempty"`
	Result          string               `json:"result"`
	ReasonCode      string               `json:"reason_code,omitempty"`
	RiskLevel       string               `json:"risk_level,omitempty"`
	Classification  string               `json:"classification,omitempty"`
	Summary         string               `json:"summary,omitempty"`
	Metadata        map[string]any       `json:"metadata,omitempty"`
	ChangeSummary   []domain.FieldChange `json:"change_summary,omitempty"`
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

// ingestionValidationResponse is deliberately credential-free.  It lets an
// audit publisher prove that the platform accepted its bearer token and source
// binding without ever exposing a token, secret, or authorization internals.
type ingestionValidationResponse struct {
	Status          string `json:"status"`
	ApplicationCode string `json:"application_code"`
	EnvironmentCode string `json:"environment_code"`
	ClientID        string `json:"client_id"`
	AuditIngest     bool   `json:"audit_ingest"`
}

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

// ValidateIngestion verifies the OAuth application principal at the same
// boundary as ingestion. It is intentionally read-only and does not create an
// audit receipt, so deployment probes can detect an application/environment/
// client mismatch before an Outbox starts accumulating retries.
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

// DownloadExport streams a completed tenant-scoped export from the local storage root.
// Storage paths are never accepted from the request and are validated before opening a file.
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
	query := application.PageRequest{Page: parseInt(r.URL.Query().Get("page"), 1), PageSize: parseInt(r.URL.Query().Get("page_size"), 20), Keyword: strings.TrimSpace(r.URL.Query().Get("keyword")), ApplicationCode: strings.TrimSpace(r.URL.Query().Get("filter[application_code]")), EnvironmentCode: strings.TrimSpace(r.URL.Query().Get("filter[environment_code]")), Action: strings.TrimSpace(r.URL.Query().Get("filter[action]")), ActionCategory: strings.TrimSpace(r.URL.Query().Get("filter[action_category]")), Result: strings.TrimSpace(r.URL.Query().Get("filter[result]")), RiskLevel: strings.TrimSpace(r.URL.Query().Get("filter[risk_level]"))}
	query.OccurredFrom = parseTime(r.URL.Query().Get("filter[occurred_from]"))
	query.OccurredTo = parseTime(r.URL.Query().Get("filter[occurred_to]"))
	return query
}

// applicationInput requires an explicit source binding from an application bearer token.
// The external payload never controls client_id: it is injected only from the verified principal.
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

	input := toInput(payload, r)
	input.ClientID = principal.ClientID
	return input, nil
}

// batchDeliveryInput captures trusted transport metadata for one Outbox delivery. These values are
// stored independently from every event's original operation correlation fields.
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

// validateExternalCorrelation enforces the published subsystem audit contract. A single-event
// request must repeat the trusted transport triplet in its body. A batch request keeps one
// delivery-level transport triplet while every event carries its original operation triplet.
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

func toInput(payload eventInputPayload, r *http.Request) application.EventInput {
	metadata := copyMetadata(payload.Metadata)
	// method and path can describe the audited subsystem operation. Keep supplied values and add
	// receiver values only when the reporting application did not provide them.
	if _, ok := metadata["method"]; !ok {
		metadata["method"] = r.Method
	}
	if _, ok := metadata["path"]; !ok {
		metadata["path"] = r.URL.Path
	}
	return application.EventInput{EventID: payload.EventID, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.EnvironmentCode, ActorType: payload.ActorType, ActorID: payload.ActorID, ActorName: payload.ActorName, SessionID: payload.SessionID, OccurredAt: payload.OccurredAt, Action: payload.Action, ResourceType: payload.ResourceType, ResourceID: payload.ResourceID, ResourceName: payload.ResourceName, BusinessID: payload.BusinessID, RequestID: payload.RequestID, TraceID: payload.TraceID, CorrelationID: payload.CorrelationID, Result: payload.Result, ReasonCode: payload.ReasonCode, RiskLevel: payload.RiskLevel, Classification: payload.Classification, Summary: payload.Summary, Metadata: metadata, Changes: payload.ChangeSummary, SourceIP: clientIP(r), UserAgent: r.UserAgent()}
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
	return eventResponse{EventID: event.EventID, OccurredAt: event.OccurredAt, OperatorDisplayName: event.OperatorDisplayName, ActionType: event.ActionType, ApplicationCode: event.ApplicationCode, ApplicationName: event.ApplicationName, EnvironmentCode: event.EnvironmentCode, Action: event.Action, Result: event.Result, ResourceType: event.ResourceType, ResourceID: event.ResourceID, ResourceName: event.ResourceName, Method: event.Method, Path: event.Path, ClientIP: event.ClientIP, UserAgent: event.UserAgent, RequestID: event.RequestID, TraceID: event.TraceID, CorrelationID: event.CorrelationID, StatusCode: event.StatusCode, RiskLevel: event.RiskLevel, Detail: event.Detail, Summary: event.Summary, ChangeSummary: event.ChangeSummary}
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
