// Package domain defines the file metadata and asynchronous job models owned by the filetask module.
package domain

import (
	"encoding/json"
	"time"
)

const (
	FileStatusUploading   = "UPLOADING"
	FileStatusAvailable   = "AVAILABLE"
	FileStatusQuarantined = "QUARANTINED"
	FileStatusDeleting    = "DELETING"
	FileStatusDeleted     = "DELETED"
	FileStatusFailed      = "FAILED"

	FileVersionStatusWriting = "WRITING"
	FileVersionStatusReady   = "AVAILABLE"
	FileVersionStatusFailed  = "FAILED"
	FileVersionStatusRemoved = "REMOVED"

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
	CreatedAt                       time.Time
}

// StoredFile joins a file and its current binary version for secure download and cleanup work.
type StoredFile struct {
	File    File
	Version FileVersion
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
	TenantID         string
	ApplicationID    string
	JobType          string
	AggregateType    string
	AggregateID      string
	Payload          json.RawMessage
	Status           string
	Priority         int
	AvailableAt      time.Time
	LockedBy         string
	LockedAt         *time.Time
	Attempts         uint
	MaxAttempts      uint
	LastErrorCode    string
	LastErrorMessage string
	ResultFileID     string
	CreatedAt        time.Time
	CompletedAt      *time.Time
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
