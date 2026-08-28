// Package domain defines the file metadata and asynchronous job models owned by the filetask module.
package domain

import (
	"encoding/json"
	"time"
)

const (
	FileStatusPendingUpload = "PENDING_UPLOAD"
	FileStatusValidating    = "VALIDATING"
	FileStatusReady         = "READY"
	FileStatusRejected      = "REJECTED"
	FileStatusQuarantined   = "QUARANTINED"
	FileStatusDeleting      = "DELETING"
	FileStatusDeleted       = "DELETED"
	FileStatusFailed        = "FAILED"

	FileVersionStatusPendingUpload = "PENDING_UPLOAD"
	FileVersionStatusValidating    = "VALIDATING"
	FileVersionStatusReady         = "READY"
	FileVersionStatusRejected      = "REJECTED"
	FileVersionStatusFailed        = "FAILED"
	FileVersionStatusRemoved       = "REMOVED"

	JobStatusPending   = "PENDING"
	JobStatusRunning   = "RUNNING"
	JobStatusSucceeded = "SUCCEEDED"
	JobStatusFailed    = "FAILED"
	JobStatusDead      = "DEAD"
	JobStatusCancelled = "CANCELLED"
)

// File contains the logical metadata for a locally stored file. Its storage path is deliberately
// absent: API callers must never receive the server-side relative path.
type File struct {
	ID, TenantID, ApplicationID string
	OriginalName, FileExtension string
	MediaType, Classification   string
	OwnerUserID                 string
	CurrentVersionNo            uint
	CurrentVersionID            string
	Status                      string
	Version                     uint64
	CreatedAt, UpdatedAt        time.Time
}

// FileVersion is one immutable binary version. StorageRelativePath is internal-only and must not
// be returned by the HTTP adapter.
type FileVersion struct {
	ID, FileID, StorageRelativePath string
	VersionNo                       uint
	SizeBytes                       uint64
	SHA256                          []byte
	MediaType, OriginalName, Status string
	UploaderUserID, UploadRequestID string
	UploadRequestHash               []byte
	CreatedAt                       time.Time
}

// StoredFile joins a file and its current binary version for secure download and cleanup work.
type StoredFile struct {
	File    File
	Version FileVersion
}

// FileBinding 将文件关联到业务子系统内的具体资源。绑定只保存稳定标识，平台不解释
// resource_type 和 resource_id 的业务语义；子系统仍负责判断当前用户能否访问该资源。
type FileBinding struct {
	ID, TenantID, ApplicationID string
	FileID, ResourceType        string
	ResourceID, BindingType     string
	DisplayName                 string
	SortOrder                   int
	Status, CreatedBy           string
	CreatedAt                   time.Time
}

// PageRequest is the common, tenant-scoped operation query shape.
type PageRequest struct {
	Page, PageSize       int
	Status, JobType      string
	ApplicationID, Query string
}

// PageResult is a deterministic, pagination-friendly result set.
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// Job represents one MySQL-backed asynchronous task. Payload is intentionally opaque to the
// scheduler; only the worker registered for JobType may interpret it.
type Job struct {
	ID               uint64
	PublicID         string
	ParentJobID      uint64
	ParentPublicID   string
	TenantID         string
	ApplicationID    string
	JobType          string
	AggregateType    string
	AggregateID      string
	IdempotencyKey   string
	Payload          json.RawMessage
	RequestHash      []byte
	RequestID        string
	TraceID          string
	CorrelationID    string
	BusinessRef      string
	Status           string
	Priority         int
	AvailableAt      time.Time
	LockedBy         string
	LockedAt         *time.Time
	LastAttemptAt    *time.Time
	Attempts         uint
	RetryCount       uint
	MaxAttempts      uint
	LastErrorCode    string
	LastErrorMessage string
	ResultFileID     string
	CreatedAt        time.Time
	CompletedAt      *time.Time
	LastSucceededAt  *time.Time
}

// CleanupResult describes the bounded local-file cleanup pass. It purposely exposes counts only,
// not physical paths or file contents.
type CleanupResult struct {
	ClaimedFiles       int `json:"claimed_files"`
	DeletedFiles       int `json:"deleted_files"`
	FailedFiles        int `json:"failed_files"`
	RemovedTempFiles   int `json:"removed_temp_files"`
	TempCleanupFailure int `json:"temp_cleanup_failure"`
}

// ReconcileResult 汇总一次有界文件状态对账，不暴露物理路径或内容。
type ReconcileResult struct {
	Inspected int `json:"inspected"`
	Recovered int `json:"recovered"`
	Rejected  int `json:"rejected"`
	Failed    int `json:"failed"`
	Conflicts int `json:"conflicts"`
}
