// Package domain contains runtime telemetry and alerting models. They are deliberately
// independent from immutable business audit events.
package domain

import "time"

const (
	SeverityDebug = "DEBUG"
	SeverityInfo  = "INFO"
	SeverityWarn  = "WARN"
	SeverityError = "ERROR"

	AlertRuleEnabled  = "ENABLED"
	AlertRuleDisabled = "DISABLED"

	// AlertStateOK means a rule is not currently violating its threshold.
	AlertStateOK = "OK"
	// AlertStateFiring means the current observation violates a rule threshold.
	AlertStateFiring = "FIRING"
	// AlertStateRecovered is persisted for the evaluation that ends a previous firing state.
	// The rule itself is subsequently stored as OK.
	AlertStateRecovered = "RECOVERED"

	NotificationStatusNotRequired = "NOT_REQUIRED"
	NotificationStatusSent        = "SENT"
	NotificationStatusFailed      = "FAILED"
	NotificationStatusSuppressed  = "SUPPRESSED"
)

// ResourceLabels identifies the runtime origin required by the observability convention.
// TenantID and ApplicationID are required before a telemetry record can be stored.
type ResourceLabels struct {
	ServiceName           string
	ServiceVersion        string
	DeploymentEnvironment string
	ServiceInstanceID     string
	ApplicationID         string
	TenantID              string
}

// LogRecord is a structured diagnostic record. It deliberately has no business audit semantics.
type LogRecord struct {
	Timestamp   time.Time
	Severity    string
	Message     string
	TraceID     string
	SpanID      string
	RequestID   string
	Application string
	Module      string
	Operation   string
	ErrorCode   string
	DurationMS  float64
	Resource    ResourceLabels
	Attributes  map[string]any
}

// TraceSpan is a compact presentation/query model. OTLP collectors remain the long-term trace
// backend, while this model supports bounded local diagnostics without writing trace data to MySQL.
type TraceSpan struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Name          string
	Kind          string
	StatusCode    string
	StatusMessage string
	StartedAt     time.Time
	EndedAt       time.Time
	Resource      ResourceLabels
	Attributes    map[string]any
}

// MetricPoint keeps one metric observation in a bounded local buffer rather than MySQL.
type MetricPoint struct {
	Timestamp  time.Time
	Name       string
	Unit       string
	TraceID    string
	Value      float64
	Resource   ResourceLabels
	Attributes map[string]string
}

// AlertRule is persistent metadata for one metric threshold; it does not store telemetry samples.
type AlertRule struct {
	RuleID          string
	TenantID        string
	ApplicationID   string
	Name            string
	MetricName      string
	Comparator      string
	Severity        string
	Status          string
	Threshold       float64
	WindowSeconds   uint
	CreatedBy       string
	UpdatedBy       string
	LastState       string
	LastTriggeredAt time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         uint
}

// AlertEvaluation records one threshold decision and its notification outcome. It contains only
// aggregate metric metadata and never persists telemetry attributes or raw request data.
type AlertEvaluation struct {
	EvaluationID       string
	RuleID             string
	TenantID           string
	State              string
	NotificationStatus string
	ErrorMessage       string
	ObservedValue      float64
	EvaluatedAt        time.Time
}

// AlertExecution is returned to operations after executing one rule. Suppressed indicates that a
// continuing firing state was intentionally not notified because its repeat interval has not elapsed.
type AlertExecution struct {
	Rule       AlertRule
	Evaluation AlertEvaluation
	Triggered  bool
	Recovered  bool
	Suppressed bool
}
