// Package application coordinates append-only audit ingestion, querying and export job creation.
package application

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
)

var (
	correlationIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	traceIDPattern               = regexp.MustCompile(`^[0-9a-f]{32}$`)

	ErrNotFound   = errors.New("audit resource not found")
	ErrConflict   = errors.New("audit resource conflict")
	ErrValidation = errors.New("audit validation failed")
)

type IdentifierGenerator interface {
	New(time.Time) (string, error)
}
type Clock interface{ Now() time.Time }
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type PageRequest struct {
	Page, PageSize                                                                       int
	Keyword, ApplicationCode, EnvironmentCode, Action, ActionCategory, Result, RiskLevel string
	OccurredFrom, OccurredTo                                                             *time.Time
}
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

type EventInput struct {
	EventID, ApplicationCode, EnvironmentCode, ActorType, ActorID, ActorName, SessionID, ClientID string
	OccurredAt                                                                                    time.Time
	Action, ResourceType, ResourceID, ResourceName, BusinessID, RequestID, TraceID, CorrelationID string
	Result, ReasonCode, RiskLevel, Classification, Summary                                        string
	Metadata                                                                                      map[string]any
	Changes                                                                                       []domain.FieldChange
	SourceIP, UserAgent, EventCategory, EventType                                                 string
}

// BatchDeliveryInput identifies one transport delivery of one or more audit events. Its
// correlation triplet belongs to the receiver delivery chain; EventInput keeps each audited
// operation's original correlation triplet.
type BatchDeliveryInput struct {
	ApplicationCode, EnvironmentCode, ClientID string
	RequestID, TraceID, CorrelationID          string
	SourceIP, UserAgent                        string
}

type Repository interface {
	Ingest(context.Context, string, EventInput, time.Time) (domain.Receipt, error)
	IngestBatch(context.Context, string, BatchDeliveryInput, []EventInput, time.Time) ([]domain.Receipt, error)
	List(context.Context, string, PageRequest) (PageResult[domain.Event], error)
	Get(context.Context, string, string) (domain.Event, error)
	CreateExportJob(context.Context, string, string, PageRequest, string, time.Time) (domain.ExportJob, error)
	GetExportJob(context.Context, string, string) (domain.ExportJob, error)
	ClaimExportJob(context.Context, string, time.Time, time.Time) (domain.ExportWork, bool, error)
	ListExportEvents(context.Context, string, domain.ExportQuery) ([]domain.Event, error)
	CompleteExportJob(context.Context, domain.ExportWork, domain.ExportFile, time.Time) error
	FailExportJob(context.Context, domain.ExportWork, string, string, time.Time, time.Time) error
	GetExportFile(context.Context, string, string) (domain.ExportFile, error)
}

type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("audit service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, clock: clock}, nil
}
func (s *Service) Ingest(ctx context.Context, tenantID string, input EventInput) (domain.Receipt, error) {
	input = completeCorrelation(ctx, input)
	if err := validateInput(tenantID, input); err != nil {
		return domain.Receipt{}, err
	}
	return s.repository.Ingest(ctx, tenantID, normalizeInput(input), s.clock.Now())
}

// IngestBatch validates every event before persistence and delegates the accepted/duplicate
// decision to one repository transaction. Any validation or storage failure therefore produces no
// batch delivery receipt and no partial event write; duplicates remain successful per-event results.
func (s *Service) IngestBatch(ctx context.Context, tenantID string, delivery BatchDeliveryInput, inputs []EventInput) ([]domain.Receipt, error) {
	if len(inputs) == 0 || len(inputs) > 100 {
		return nil, validation("events must contain 1 to 100 items")
	}
	if err := validateBatchDelivery(tenantID, delivery); err != nil {
		return nil, err
	}

	normalized := make([]EventInput, 0, len(inputs))
	for _, input := range inputs {
		if err := validateInput(tenantID, input); err != nil {
			return nil, err
		}
		if input.ApplicationCode != delivery.ApplicationCode || input.EnvironmentCode != delivery.EnvironmentCode || input.ClientID != delivery.ClientID {
			return nil, validation("batch event source does not match delivery principal")
		}
		normalized = append(normalized, normalizeInput(input))
	}
	return s.repository.IngestBatch(ctx, tenantID, normalizeBatchDelivery(delivery), normalized, s.clock.Now())
}
func (s *Service) List(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Event], error) {
	query = normalizePage(query)
	if !validActionCategory(query.ActionCategory) {
		return PageResult[domain.Event]{}, validation("action_category is invalid")
	}
	return s.repository.List(ctx, tenantID, query)
}
func (s *Service) Get(ctx context.Context, tenantID, eventID string) (domain.Event, error) {
	if strings.TrimSpace(eventID) == "" {
		return domain.Event{}, validation("event_id is required")
	}
	return s.repository.Get(ctx, tenantID, strings.TrimSpace(eventID))
}
func (s *Service) CreateExportJob(ctx context.Context, tenantID, operatorID string, query PageRequest) (domain.ExportJob, error) {
	if strings.TrimSpace(operatorID) == "" {
		return domain.ExportJob{}, validation("operator is required")
	}
	query = normalizePage(query)
	if !validActionCategory(query.ActionCategory) {
		return domain.ExportJob{}, validation("action_category is invalid")
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.ExportJob{}, fmt.Errorf("generate export job ID: %w", err)
	}
	return s.repository.CreateExportJob(ctx, tenantID, operatorID, query, id, s.clock.Now())
}
func (s *Service) GetExportJob(ctx context.Context, tenantID, jobID string) (domain.ExportJob, error) {
	if strings.TrimSpace(jobID) == "" {
		return domain.ExportJob{}, validation("job_id is required")
	}
	return s.repository.GetExportJob(ctx, tenantID, strings.TrimSpace(jobID))
}
func normalizePage(query PageRequest) PageRequest {
	query.ActionCategory = strings.ToUpper(strings.TrimSpace(query.ActionCategory))
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	return query
}

func validActionCategory(category string) bool {
	return category == "" || oneOf(category, "LOGIN", "CREATE", "UPDATE", "EXPORT", "STATUS_CHANGE")
}

// completeCorrelation fills only missing event correlation fields from the trusted request
// context. A reporting subsystem's own request_id and trace_id describe the audited operation and
// are therefore retained when supplied. Source application, environment, client, address, and user
// agent are bound by the HTTP adapter rather than by this helper.
func completeCorrelation(ctx context.Context, input EventInput) EventInput {
	input.RequestID = strings.TrimSpace(input.RequestID)
	input.TraceID = strings.TrimSpace(input.TraceID)
	input.CorrelationID = strings.TrimSpace(input.CorrelationID)

	if input.RequestID == "" {
		input.RequestID = strings.TrimSpace(requestctx.RequestID(ctx))
	}
	if input.TraceID == "" {
		input.TraceID = strings.TrimSpace(requestctx.TraceID(ctx))
	}
	if input.CorrelationID == "" {
		input.CorrelationID = strings.TrimSpace(requestctx.CorrelationID(ctx))
	}
	return input
}

func validateBatchDelivery(tenantID string, delivery BatchDeliveryInput) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(delivery.ApplicationCode) == "" || strings.TrimSpace(delivery.EnvironmentCode) == "" || strings.TrimSpace(delivery.ClientID) == "" {
		return validation("tenant, application_code, environment_code and client_id are required for batch delivery")
	}
	if strings.TrimSpace(delivery.RequestID) == "" || strings.TrimSpace(delivery.TraceID) == "" || strings.TrimSpace(delivery.CorrelationID) == "" {
		return validation("delivery request_id, trace_id and correlation_id are required")
	}
	if len(delivery.ClientID) > 128 || len(delivery.RequestID) > 128 || len(delivery.CorrelationID) > 128 {
		return validation("batch delivery field exceeds storage limit")
	}
	if !correlationIdentifierPattern.MatchString(delivery.RequestID) {
		return validation("delivery request_id is invalid")
	}
	if !correlationIdentifierPattern.MatchString(delivery.CorrelationID) {
		return validation("delivery correlation_id is invalid")
	}
	if !traceIDPattern.MatchString(delivery.TraceID) || delivery.TraceID == strings.Repeat("0", 32) {
		return validation("delivery trace_id must be a non-zero 32-character lowercase hexadecimal value")
	}
	return nil
}

func normalizeBatchDelivery(delivery BatchDeliveryInput) BatchDeliveryInput {
	delivery.ApplicationCode = strings.TrimSpace(delivery.ApplicationCode)
	delivery.EnvironmentCode = strings.TrimSpace(delivery.EnvironmentCode)
	delivery.ClientID = strings.TrimSpace(delivery.ClientID)
	delivery.RequestID = strings.TrimSpace(delivery.RequestID)
	delivery.TraceID = strings.TrimSpace(delivery.TraceID)
	delivery.CorrelationID = strings.TrimSpace(delivery.CorrelationID)
	delivery.SourceIP = strings.TrimSpace(delivery.SourceIP)
	delivery.UserAgent = strings.TrimSpace(delivery.UserAgent)
	return delivery
}

func validateInput(tenantID string, input EventInput) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(input.EventID) == "" || strings.TrimSpace(input.ApplicationCode) == "" || strings.TrimSpace(input.EnvironmentCode) == "" || input.OccurredAt.IsZero() || strings.TrimSpace(input.Action) == "" || strings.TrimSpace(input.Result) == "" {
		return validation("event_id, application_code, environment_code, occurred_at, action and result are required")
	}
	if len(input.EventID) > 128 || len(input.Action) > 128 || len(input.Summary) > 1000 || len(input.RequestID) > 128 || len(input.CorrelationID) > 128 {
		return validation("audit field exceeds storage limit")
	}
	if input.RequestID != "" && !correlationIdentifierPattern.MatchString(input.RequestID) {
		return validation("request_id is invalid")
	}
	if input.CorrelationID != "" && !correlationIdentifierPattern.MatchString(input.CorrelationID) {
		return validation("correlation_id is invalid")
	}
	if input.TraceID != "" && (!traceIDPattern.MatchString(input.TraceID) || input.TraceID == strings.Repeat("0", 32)) {
		return validation("trace_id must be a non-zero 32-character lowercase hexadecimal value")
	}
	if !oneOf(input.Result, "SUCCESS", "FAILURE", "DENIED") {
		return validation("result is invalid")
	}
	if input.ResourceType == "" {
		return validation("resource_type is required")
	}
	if input.RiskLevel != "" && !oneOf(input.RiskLevel, "LOW", "MEDIUM", "HIGH", "CRITICAL") {
		return validation("risk_level is invalid")
	}
	return nil
}
func normalizeInput(input EventInput) EventInput {
	input.EventID = strings.TrimSpace(input.EventID)
	input.ApplicationCode = strings.TrimSpace(input.ApplicationCode)
	input.EnvironmentCode = strings.TrimSpace(input.EnvironmentCode)
	input.Action = strings.TrimSpace(input.Action)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.Result = strings.TrimSpace(input.Result)
	if input.ActorType == "" {
		input.ActorType = "SYSTEM"
	}
	if input.RiskLevel == "" {
		input.RiskLevel = "LOW"
	}
	if input.Classification == "" {
		input.Classification = "INTERNAL"
	}
	if input.EventCategory == "" {
		input.EventCategory = "EXTERNAL"
	}
	if input.EventType == "" {
		input.EventType = input.Action
	}
	return input
}
func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

// ClaimExportJob claims one due export job for the named worker. The returned work is owned by
// that worker until it is completed or failed.
func (s *Service) ClaimExportJob(ctx context.Context, workerID string, staleBefore time.Time) (domain.ExportWork, bool, error) {
	if strings.TrimSpace(workerID) == "" {
		return domain.ExportWork{}, false, validation("worker ID is required")
	}
	return s.repository.ClaimExportJob(ctx, strings.TrimSpace(workerID), s.clock.Now(), staleBefore.UTC())
}

// ListExportEvents returns the complete, filtered event set used by the local export worker.
func (s *Service) ListExportEvents(ctx context.Context, tenantID string, query domain.ExportQuery) ([]domain.Event, error) {
	return s.repository.ListExportEvents(ctx, tenantID, query)
}

func (s *Service) CompleteExportJob(ctx context.Context, work domain.ExportWork, file domain.ExportFile) error {
	return s.repository.CompleteExportJob(ctx, work, file, s.clock.Now())
}

func (s *Service) FailExportJob(ctx context.Context, work domain.ExportWork, code, message string, retryAt time.Time) error {
	return s.repository.FailExportJob(ctx, work, strings.TrimSpace(code), strings.TrimSpace(message), retryAt.UTC(), s.clock.Now())
}

// GetExportFile loads the local file metadata for a completed job without exposing its physical
// path to API consumers.
func (s *Service) GetExportFile(ctx context.Context, tenantID, jobID string) (domain.ExportFile, error) {
	if strings.TrimSpace(jobID) == "" {
		return domain.ExportFile{}, validation("job_id is required")
	}
	return s.repository.GetExportFile(ctx, tenantID, strings.TrimSpace(jobID))
}
