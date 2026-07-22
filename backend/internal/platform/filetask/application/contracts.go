// Package application coordinates secure local file storage and generic MySQL asynchronous jobs.
package application

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/domain"
)

// FileRepository persists file metadata. The file system is intentionally kept behind LocalStore
// because MySQL and the local file system cannot share one distributed transaction.
type FileRepository interface {
	CreateWriting(context.Context, domain.File, domain.FileVersion) error
	MarkAvailable(context.Context, string, string, uint64, []byte, time.Time) error
	MarkFailed(context.Context, string, string, time.Time) error
	GetAvailable(context.Context, string, string) (domain.StoredFile, error)
	ClaimExpiredUnbound(context.Context, string, time.Time) (domain.StoredFile, bool, error)
	MarkDeleted(context.Context, string, string, time.Time) error
	ReleaseCleanupClaim(context.Context, string, string, string, time.Time) error
}

// JobRepository owns generic async_job state transitions. It must implement claims in short
// transactions with row locks; workers never hold a database lock while doing slow work.
type JobRepository interface {
	CreateJob(context.Context, domain.Job) (domain.Job, error)
	ListJobs(context.Context, string, domain.PageRequest) (domain.PageResult[domain.Job], error)
	GetJob(context.Context, string, string) (domain.Job, error)
	ClaimJob(context.Context, string, []string, time.Time, time.Time) (domain.Job, bool, error)
	CompleteJob(context.Context, string, string, time.Time) error
	FailJob(context.Context, domain.Job, string, string, bool, time.Time, time.Time) error
	CancelJob(context.Context, string, string, time.Time) error
	RetryJob(context.Context, string, string, time.Time) error
	CreateRerun(context.Context, domain.Job) (domain.Job, error)
}

// LocalStore writes and opens files below one trusted root. Implementations must reject path
// traversal and symlink escapes even when storage metadata has been corrupted.
type LocalStore interface {
	WriteAtomically(context.Context, string, io.Reader, int64) (size uint64, sha256 []byte, err error)
	OpenVerified(string) (io.ReadSeekCloser, error)
	Remove(string) error
	CleanupTemporary(time.Time) (int, error)
}

// IDGenerator gives the application layer monotonic public identifiers without coupling it to a
// database implementation.
type IDGenerator interface {
	New(time.Time) (string, error)
}

// Clock makes expiry and retry behavior deterministic in module tests.
type Clock interface{ Now() time.Time }

// SystemClock is the production UTC clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// UploadPolicy controls the module's upload boundary. The platform deliberately uses an allowlist
// rather than trusting browser MIME headers.
type UploadPolicy struct {
	MaxBytes          int64
	AllowedMediaTypes map[string]struct{}
}

// UploadInput is request-local binary input. Content is consumed exactly once and is never logged.
type UploadInput struct {
	TenantID, ApplicationID, OwnerUserID string
	OriginalName, DeclaredMediaType      string
	Classification, RequestID            string
	Content                              io.Reader
}

// DownloadAccess carries the server-authenticated subject and its already-resolved permissions.
// HTTP clients cannot populate it directly; the handler reads it from authctx.
type DownloadAccess struct {
	TenantID, UserID string
	PermissionCodes  []string
}

// JobCreateInput creates an application-owned background task. Payload must be valid JSON and
// must not contain passwords, tokens, raw uploaded contents, or other credential material.
type JobCreateInput struct {
	TenantID, ApplicationID string
	JobType, AggregateType  string
	AggregateID             string
	Payload                 json.RawMessage
	Priority                int
	MaxAttempts             uint
	AvailableAt             *time.Time
}
