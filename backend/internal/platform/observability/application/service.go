// Package application coordinates bounded runtime telemetry buffers and executable alert rules.
package application

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
)

const (
	PermissionLogView      = "platform:observability:log:view"
	PermissionTraceView    = "platform:observability:trace:view"
	PermissionMetricView   = "platform:observability:metric:view"
	PermissionAlertManage  = "platform:observability:alert:manage"
	PermissionAlertExecute = "platform:observability:alert:execute"

	defaultAlertReminderInterval = 15 * time.Minute
)

var (
	ErrNotFound   = errors.New("observability resource not found")
	ErrConflict   = errors.New("observability resource conflict")
	ErrValidation = errors.New("observability validation failed")
)

// IdentifierGenerator supplies sortable identifiers for alert rules and evaluations.
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock makes observability timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the UTC production clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

// RuntimeStore is a bounded, non-MySQL telemetry store. Deployments should replace it with an
// OTLP-compatible backend adapter when a collector is available.
type RuntimeStore interface {
	AppendLog(context.Context, domain.LogRecord) error
	AppendSpan(context.Context, domain.TraceSpan) error
	AppendMetric(context.Context, domain.MetricPoint) error
	QueryLogs(context.Context, LogQuery) ([]domain.LogRecord, error)
	QueryTraces(context.Context, TraceQuery) ([]domain.TraceSpan, error)
	QueryMetrics(context.Context, MetricQuery) ([]domain.MetricPoint, error)
}

// StructuredLogSink can persist a local JSON rolling stream. It is deliberately independent
// from audit tables and must never receive credentials or request bodies.
type StructuredLogSink interface {
	WriteStructuredLog(context.Context, domain.LogRecord) error
}

// LogQuery bounds tenant-scoped structured-log reads.
type LogQuery struct {
	TenantID      string
	ApplicationID string
	TraceID       string
	RequestID     string
	Severity      string
	Keyword       string
	From          *time.Time
	To            *time.Time
	Limit         int
}

// TraceQuery bounds tenant-scoped trace span reads.
type TraceQuery struct {
	TenantID      string
	ApplicationID string
	TraceID       string
	RequestID     string
	From          *time.Time
	To            *time.Time
	Limit         int
}

// MetricQuery bounds tenant-scoped metric point reads.
type MetricQuery struct {
	TenantID      string
	ApplicationID string
	Name          string
	From          *time.Time
	To            *time.Time
	Limit         int
}

// TelemetryService validates and redacts runtime data before it reaches the bounded store and
// optional structured JSON sink. It intentionally has no anonymous HTTP ingestion contract.
type TelemetryService struct {
	store RuntimeStore
	sink  StructuredLogSink
	clock Clock
}

// NewTelemetryService constructs a telemetry use-case service.
func NewTelemetryService(store RuntimeStore, sink StructuredLogSink, clock Clock) (*TelemetryService, error) {
	if store == nil || clock == nil {
		return nil, errors.New("telemetry service dependencies must not be nil")
	}
	return &TelemetryService{store: store, sink: sink, clock: clock}, nil
}

// RecordLog stores a redacted structured runtime log.
func (service *TelemetryService) RecordLog(ctx context.Context, record domain.LogRecord) error {
	record = normalizeLog(record, service.clock.Now())
	if err := validateLog(record); err != nil {
		return err
	}
	if service.sink != nil {
		if err := service.sink.WriteStructuredLog(ctx, record); err != nil {
			return err
		}
	}
	return service.store.AppendLog(ctx, record)
}

// RecordSpan stores a redacted runtime trace span.
func (service *TelemetryService) RecordSpan(ctx context.Context, span domain.TraceSpan) error {
	span = normalizeSpan(span, service.clock.Now())
	if err := validateSpan(span); err != nil {
		return err
	}
	return service.store.AppendSpan(ctx, span)
}

// RecordMetric stores one finite metric observation.
func (service *TelemetryService) RecordMetric(ctx context.Context, point domain.MetricPoint) error {
	point = normalizeMetric(point, service.clock.Now())
	if err := validateMetric(point); err != nil {
		return err
	}
	return service.store.AppendMetric(ctx, point)
}

// QueryLogs returns only the caller's tenant-scoped diagnostic records.
func (service *TelemetryService) QueryLogs(ctx context.Context, query LogQuery) ([]domain.LogRecord, error) {
	if strings.TrimSpace(query.TenantID) == "" {
		return nil, validation("tenant_id is required")
	}
	query.Limit = normalizeLimit(query.Limit)
	return service.store.QueryLogs(ctx, query)
}

// QueryTraces returns only the caller's tenant-scoped diagnostic spans.
func (service *TelemetryService) QueryTraces(ctx context.Context, query TraceQuery) ([]domain.TraceSpan, error) {
	if strings.TrimSpace(query.TenantID) == "" {
		return nil, validation("tenant_id is required")
	}
	query.Limit = normalizeLimit(query.Limit)
	return service.store.QueryTraces(ctx, query)
}

// QueryMetrics returns only the caller's tenant-scoped metric observations.
func (service *TelemetryService) QueryMetrics(ctx context.Context, query MetricQuery) ([]domain.MetricPoint, error) {
	if strings.TrimSpace(query.TenantID) == "" {
		return nil, validation("tenant_id is required")
	}
	query.Limit = normalizeLimit(query.Limit)
	return service.store.QueryMetrics(ctx, query)
}

// AlertRuleInput creates one versioned metric threshold rule.
type AlertRuleInput struct {
	TenantID      string
	ApplicationID string
	Name          string
	MetricName    string
	Comparator    string
	Severity      string
	Status        string
	OperatorID    string
	Threshold     float64
	WindowSeconds uint
}

// AlertRuleUpdateInput changes the complete mutable threshold configuration under optimistic
// locking. Rule identifiers and tenant ownership cannot be changed.
type AlertRuleUpdateInput struct {
	TenantID      string
	RuleID        string
	ApplicationID string
	Name          string
	MetricName    string
	Comparator    string
	Severity      string
	Status        string
	OperatorID    string
	Threshold     float64
	WindowSeconds uint
	Version       uint
}

// AlertServiceConfig controls notification suppression for continuing alert states. The setting
// is process configuration rather than a per-rule field because existing schema does not yet
// persist a notification interval.
type AlertServiceConfig struct {
	ReminderInterval time.Duration
}

// AlertRepository persists rule and evaluation metadata. Runtime telemetry samples remain in a
// RuntimeStore, never in business MySQL tables.
type AlertRepository interface {
	CreateAlertRule(context.Context, domain.AlertRule) (domain.AlertRule, error)
	GetAlertRule(context.Context, string, string) (domain.AlertRule, error)
	ListAlertRules(context.Context, string) ([]domain.AlertRule, error)
	ListEnabledAlertRules(context.Context) ([]domain.AlertRule, error)
	UpdateAlertRule(context.Context, domain.AlertRule, uint) (domain.AlertRule, error)
	PersistEvaluationAndRuleState(context.Context, domain.AlertRule, domain.AlertEvaluation, string, time.Time) (domain.AlertRule, error)
}

// StationMessage is the cross-module port for a rendered in-app notification. Observability does
// not import the notification package or write notification tables directly.
type StationMessage struct {
	TenantID         string
	RecipientID      string
	Category         string
	Title            string
	Content          string
	DeduplicationKey string
	Metadata         map[string]string
}

// StationNotifier is implemented by a composition-root adapter to the notification module.
type StationNotifier interface {
	PublishStationMessage(context.Context, StationMessage) error
}

// AlertService executes threshold rules on bounded metric data and uses StationNotifier on a
// firing transition, a controlled reminder, and recovery. Notification errors are persisted in
// evaluation metadata; they never prevent state transitions.
type AlertService struct {
	repository       AlertRepository
	store            RuntimeStore
	notifier         StationNotifier
	ids              IdentifierGenerator
	clock            Clock
	reminderInterval time.Duration
}

// NewAlertService constructs an alert service. A notifier is intentionally required: callers
// that have not wired in-app notifications must use an explicit adapter, not silently drop alerts.
func NewAlertService(
	repository AlertRepository,
	store RuntimeStore,
	notifier StationNotifier,
	ids IdentifierGenerator,
	clock Clock,
	configurations ...AlertServiceConfig,
) (*AlertService, error) {
	if repository == nil || store == nil || notifier == nil || ids == nil || clock == nil {
		return nil, errors.New("alert service dependencies must not be nil")
	}

	configuration := AlertServiceConfig{ReminderInterval: defaultAlertReminderInterval}
	if len(configurations) > 0 && configurations[0].ReminderInterval > 0 {
		configuration = configurations[0]
	}
	if configuration.ReminderInterval < time.Minute || configuration.ReminderInterval > 24*time.Hour {
		return nil, validation("alert reminder interval must be between one minute and 24 hours")
	}

	return &AlertService{
		repository:       repository,
		store:            store,
		notifier:         notifier,
		ids:              ids,
		clock:            clock,
		reminderInterval: configuration.ReminderInterval,
	}, nil
}

// CreateRule validates and persists a new metric threshold rule.
func (service *AlertService) CreateRule(ctx context.Context, input AlertRuleInput) (domain.AlertRule, error) {
	if err := validateRuleInput(input); err != nil {
		return domain.AlertRule{}, err
	}

	now := service.now()
	identifier, err := service.ids.New(now)
	if err != nil {
		return domain.AlertRule{}, err
	}

	rule := domain.AlertRule{
		RuleID:        identifier,
		TenantID:      strings.TrimSpace(input.TenantID),
		ApplicationID: strings.TrimSpace(input.ApplicationID),
		Name:          strings.TrimSpace(input.Name),
		MetricName:    strings.TrimSpace(input.MetricName),
		Comparator:    strings.TrimSpace(input.Comparator),
		Severity:      strings.TrimSpace(input.Severity),
		Status:        strings.TrimSpace(input.Status),
		Threshold:     input.Threshold,
		WindowSeconds: input.WindowSeconds,
		CreatedBy:     strings.TrimSpace(input.OperatorID),
		UpdatedBy:     strings.TrimSpace(input.OperatorID),
		LastState:     domain.AlertStateOK,
		CreatedAt:     now,
		UpdatedAt:     now,
		Version:       1,
	}
	return service.repository.CreateAlertRule(ctx, rule)
}

// ListRules returns tenant-scoped alert rule metadata.
func (service *AlertService) ListRules(ctx context.Context, tenantID string) ([]domain.AlertRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, validation("tenant_id is required")
	}
	return service.repository.ListAlertRules(ctx, strings.TrimSpace(tenantID))
}

// UpdateRule replaces mutable threshold fields under optimistic locking.
func (service *AlertService) UpdateRule(ctx context.Context, input AlertRuleUpdateInput) (domain.AlertRule, error) {
	if err := validateRuleUpdateInput(input); err != nil {
		return domain.AlertRule{}, err
	}

	existing, err := service.repository.GetAlertRule(ctx, strings.TrimSpace(input.TenantID), strings.TrimSpace(input.RuleID))
	if err != nil {
		return domain.AlertRule{}, err
	}
	if existing.Version != input.Version {
		return domain.AlertRule{}, ErrConflict
	}

	existing.ApplicationID = strings.TrimSpace(input.ApplicationID)
	existing.Name = strings.TrimSpace(input.Name)
	existing.MetricName = strings.TrimSpace(input.MetricName)
	existing.Comparator = strings.TrimSpace(input.Comparator)
	existing.Severity = strings.TrimSpace(input.Severity)
	existing.Status = strings.TrimSpace(input.Status)
	existing.Threshold = input.Threshold
	existing.WindowSeconds = input.WindowSeconds
	existing.UpdatedBy = strings.TrimSpace(input.OperatorID)
	existing.UpdatedAt = service.now()

	return service.repository.UpdateAlertRule(ctx, existing, input.Version)
}

// ExecuteEnabled evaluates all enabled rules. It returns completed results before a later rule
// fails so an operator can inspect progress, while the runner logs and retries on its next cycle.
func (service *AlertService) ExecuteEnabled(ctx context.Context) ([]domain.AlertExecution, error) {
	rules, err := service.repository.ListEnabledAlertRules(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]domain.AlertExecution, 0, len(rules))
	for _, rule := range rules {
		result, err := service.executeRule(ctx, rule)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// ExecuteRule evaluates one tenant-owned rule.
func (service *AlertService) ExecuteRule(ctx context.Context, tenantID, ruleID string) (domain.AlertExecution, error) {
	rule, err := service.repository.GetAlertRule(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(ruleID))
	if err != nil {
		return domain.AlertExecution{}, err
	}
	return service.executeRule(ctx, rule)
}

func (service *AlertService) executeRule(ctx context.Context, rule domain.AlertRule) (domain.AlertExecution, error) {
	if rule.Status != domain.AlertRuleEnabled {
		return domain.AlertExecution{Rule: rule}, nil
	}

	now := service.now()
	from := now.Add(-time.Duration(rule.WindowSeconds) * time.Second)
	points, err := service.store.QueryMetrics(ctx, MetricQuery{
		TenantID:      rule.TenantID,
		ApplicationID: rule.ApplicationID,
		Name:          rule.MetricName,
		From:          &from,
		To:            &now,
		Limit:         5000,
	})
	if err != nil {
		return domain.AlertExecution{}, err
	}

	observedValue := average(points)
	currentState := domain.AlertStateOK
	if len(points) > 0 && compare(observedValue, rule.Comparator, rule.Threshold) {
		currentState = domain.AlertStateFiring
	}

	evaluationState := currentState
	recovered := rule.LastState == domain.AlertStateFiring && currentState == domain.AlertStateOK
	if recovered {
		evaluationState = domain.AlertStateRecovered
	}

	evaluationID, err := service.ids.New(now)
	if err != nil {
		return domain.AlertExecution{}, err
	}
	evaluation := domain.AlertEvaluation{
		EvaluationID:       evaluationID,
		RuleID:             rule.RuleID,
		TenantID:           rule.TenantID,
		State:              evaluationState,
		NotificationStatus: domain.NotificationStatusNotRequired,
		ObservedValue:      observedValue,
		EvaluatedAt:        now,
	}

	triggered, reminder, suppressed := service.notificationDecision(rule, currentState, recovered, now)
	if triggered || reminder || recovered {
		message := service.notificationMessage(rule, observedValue, currentState, recovered, now)
		if err := service.notifier.PublishStationMessage(ctx, message); err != nil {
			evaluation.NotificationStatus = domain.NotificationStatusFailed
			evaluation.ErrorMessage = truncate(err.Error(), 1000)
		} else {
			evaluation.NotificationStatus = domain.NotificationStatusSent
			if currentState == domain.AlertStateFiring {
				rule.LastTriggeredAt = now
			}
		}
	} else if suppressed {
		evaluation.NotificationStatus = domain.NotificationStatusSuppressed
	}

	updatedRule, err := service.repository.PersistEvaluationAndRuleState(ctx, rule, evaluation, currentState, now)
	if err != nil {
		return domain.AlertExecution{}, err
	}

	return domain.AlertExecution{
		Rule:       updatedRule,
		Evaluation: evaluation,
		Triggered:  triggered,
		Recovered:  recovered,
		Suppressed: suppressed,
	}, nil
}

func (service *AlertService) notificationDecision(rule domain.AlertRule, currentState string, recovered bool, now time.Time) (triggered, reminder, suppressed bool) {
	if recovered {
		return false, false, false
	}
	if currentState != domain.AlertStateFiring {
		return false, false, false
	}
	if rule.LastState != domain.AlertStateFiring {
		return true, false, false
	}
	if rule.LastTriggeredAt.IsZero() || !now.Before(rule.LastTriggeredAt.Add(service.reminderInterval)) {
		return false, true, false
	}
	return false, false, true
}

func (service *AlertService) notificationMessage(rule domain.AlertRule, observedValue float64, currentState string, recovered bool, now time.Time) StationMessage {
	metadata := map[string]string{
		"rule_id":        rule.RuleID,
		"metric_name":    rule.MetricName,
		"severity":       rule.Severity,
		"observed_value": fmt.Sprintf("%.4f", observedValue),
		"state":          currentState,
	}
	if recovered {
		return StationMessage{
			TenantID:         rule.TenantID,
			RecipientID:      rule.CreatedBy,
			Category:         "OBSERVABILITY_ALERT_RECOVERED",
			Title:            "可观测性告警已恢复：" + rule.Name,
			Content:          fmt.Sprintf("指标 %s 当前值 %.4f，已恢复到规则阈值范围内。", rule.MetricName, observedValue),
			DeduplicationKey: rule.RuleID + ":recovered:" + now.Format("200601021504"),
			Metadata:         metadata,
		}
	}

	return StationMessage{
		TenantID:         rule.TenantID,
		RecipientID:      rule.CreatedBy,
		Category:         "OBSERVABILITY_ALERT",
		Title:            "可观测性告警：" + rule.Name,
		Content:          fmt.Sprintf("指标 %s 当前值 %.4f，规则 %s %.4f。", rule.MetricName, observedValue, rule.Comparator, rule.Threshold),
		DeduplicationKey: rule.RuleID + ":firing:" + now.Format("200601021504"),
		Metadata:         metadata,
	}
}

func (service *AlertService) now() time.Time {
	return service.clock.Now().UTC().Truncate(time.Millisecond)
}

func normalizeLog(record domain.LogRecord, now time.Time) domain.LogRecord {
	if record.Timestamp.IsZero() {
		record.Timestamp = now.UTC()
	}
	record.Severity = strings.ToUpper(strings.TrimSpace(record.Severity))
	record.Attributes = redactAny(record.Attributes)
	return record
}

func normalizeSpan(span domain.TraceSpan, now time.Time) domain.TraceSpan {
	if span.StartedAt.IsZero() {
		span.StartedAt = now.UTC()
	}
	if span.EndedAt.IsZero() {
		span.EndedAt = span.StartedAt
	}
	span.Attributes = redactAny(span.Attributes)
	return span
}

func normalizeMetric(point domain.MetricPoint, now time.Time) domain.MetricPoint {
	if point.Timestamp.IsZero() {
		point.Timestamp = now.UTC()
	}
	return point
}

func validateLog(record domain.LogRecord) error {
	if !validResource(record.Resource) || strings.TrimSpace(record.Message) == "" ||
		!oneOf(record.Severity, domain.SeverityDebug, domain.SeverityInfo, domain.SeverityWarn, domain.SeverityError) {
		return validation("tenant/application/message/severity are required")
	}
	return nil
}

func validateSpan(span domain.TraceSpan) error {
	if !validResource(span.Resource) || strings.TrimSpace(span.TraceID) == "" || strings.TrimSpace(span.SpanID) == "" || span.EndedAt.Before(span.StartedAt) {
		return validation("valid trace span identity and duration are required")
	}
	return nil
}

func validateMetric(point domain.MetricPoint) error {
	if !validResource(point.Resource) || strings.TrimSpace(point.Name) == "" || math.IsNaN(point.Value) || math.IsInf(point.Value, 0) {
		return validation("valid tenant/application/name/value are required")
	}
	return nil
}

func validResource(resource domain.ResourceLabels) bool {
	return strings.TrimSpace(resource.TenantID) != "" && strings.TrimSpace(resource.ApplicationID) != ""
}

func validateRuleInput(input AlertRuleInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.ApplicationID) == "" ||
		strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.MetricName) == "" ||
		strings.TrimSpace(input.OperatorID) == "" || !oneOf(strings.TrimSpace(input.Comparator), "GT", "GTE", "LT", "LTE") ||
		!oneOf(strings.TrimSpace(input.Severity), "LOW", "MEDIUM", "HIGH", "CRITICAL") ||
		!oneOf(strings.TrimSpace(input.Status), domain.AlertRuleEnabled, domain.AlertRuleDisabled) ||
		input.WindowSeconds < 10 || input.WindowSeconds > 86400 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) {
		return validation("invalid alert rule")
	}
	return nil
}

func validateRuleUpdateInput(input AlertRuleUpdateInput) error {
	return validateRuleInput(AlertRuleInput{
		TenantID:      input.TenantID,
		ApplicationID: input.ApplicationID,
		Name:          input.Name,
		MetricName:    input.MetricName,
		Comparator:    input.Comparator,
		Severity:      input.Severity,
		Status:        input.Status,
		OperatorID:    input.OperatorID,
		Threshold:     input.Threshold,
		WindowSeconds: input.WindowSeconds,
	})
}

func average(points []domain.MetricPoint) float64 {
	if len(points) == 0 {
		return 0
	}
	var sum float64
	for _, point := range points {
		sum += point.Value
	}
	return sum / float64(len(points))
}

func compare(value float64, comparator string, threshold float64) bool {
	switch comparator {
	case "GT":
		return value > threshold
	case "GTE":
		return value >= threshold
	case "LT":
		return value < threshold
	case "LTE":
		return value <= threshold
	default:
		return false
	}
}

func normalizeLimit(limit int) int {
	if limit < 1 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func validation(message string) error {
	return fmt.Errorf("%w: %s", ErrValidation, message)
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func redactAny(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}

	result := make(map[string]any, len(values))
	for key, value := range values {
		if sensitive(key) {
			result[key] = "***"
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			result[key] = redactAny(typed)
		case []any:
			copyOfValues := make([]any, len(typed))
			for index, item := range typed {
				if nested, ok := item.(map[string]any); ok {
					copyOfValues[index] = redactAny(nested)
				} else {
					copyOfValues[index] = item
				}
			}
			result[key] = copyOfValues
		default:
			result[key] = value
		}
	}
	return result
}

func sensitive(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "password") || strings.Contains(key, "token") ||
		strings.Contains(key, "secret") || strings.Contains(key, "cookie") ||
		strings.Contains(key, "authorization")
}
