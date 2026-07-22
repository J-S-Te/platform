// Package domain contains audit aggregates that are safe to expose through the audit console.
package domain

import "time"

// Event is an append-only audit record. Internal correlation and subject fields are intentionally
// kept separate from console DTOs by the HTTP adapter.
type Event struct {
	ID, EventID, TenantID, ApplicationID, ApplicationCode, ApplicationName string
	EnvironmentID, EnvironmentCode                                         string
	OccurredAt                                                             time.Time
	OperatorDisplayName, ActionType, Action, Result                        string
	ResourceType, ResourceID, ResourceName                                 string
	Method, Path, ClientIP, RiskLevel, Detail, Summary                     string
	StatusCode                                                             int
	ChangeSummary                                                          []FieldChange
}

// FieldChange contains a redacted change summary. It must not contain credential material.
type FieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// Receipt reports whether an ingestion event was accepted or previously deduplicated.
type Receipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// ExportJob represents an asynchronously processed audit export.
type ExportJob struct {
	JobID, Status, DownloadURL string
	CreatedAt, CompletedAt     time.Time
}

// ExportWork is an audit export job claimed by one local worker process.
type ExportWork struct {
	JobID, TenantID, ApplicationID, OperatorID string
	Query                                      ExportQuery
	Attempts, MaxAttempts                      uint
}

// ExportQuery is the persisted filter set used to produce an audit export.
type ExportQuery struct {
	Keyword, ApplicationCode, EnvironmentCode, Action, Result, RiskLevel string
	OccurredFrom, OccurredTo                                             *time.Time
}

// ExportFile describes the local file attached to a completed audit export job.
type ExportFile struct {
	FileID, VersionID, StorageRelativePath, OriginalName, MediaType string
	SizeBytes                                                       uint64
	SHA256                                                          []byte
	CreatedAt                                                       time.Time
}
