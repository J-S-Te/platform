// Package domain 定义 filetask 模块拥有的文件元数据与异步任务模型。
package domain

import (
	"encoding/json"
	"time"
)

// 文件逻辑状态机：上传中、校验中、可用、被拒、隔离、删除中、已删除、失败与过期。
// 每次状态迁移都必须有对应的审计记录。
const (
	FileStatusPendingUpload = "PENDING_UPLOAD"
	FileStatusValidating    = "VALIDATING"
	FileStatusReady         = "READY"
	FileStatusRejected      = "REJECTED"
	FileStatusQuarantined   = "QUARANTINED"
	FileStatusDeleting      = "DELETING"
	FileStatusDeleted       = "DELETED"
	FileStatusFailed        = "FAILED"
	FileStatusExpired       = "EXPIRED"
)

// 保留类别：通用清理只允许 TEMPORARY；LONG_TERM 与保留期为空/未知必须保持字节不变。
const (
	RetentionClassTemporary = "TEMPORARY"
	RetentionClassLongTerm  = "LONG_TERM"
)

// 文件版本状态机：同一文件按版本独立演进，REMOVED 表示内容字节已删除但元数据保留。
const (
	FileVersionStatusPendingUpload = "PENDING_UPLOAD"
	FileVersionStatusValidating    = "VALIDATING"
	FileVersionStatusReady         = "READY"
	FileVersionStatusRejected      = "REJECTED"
	FileVersionStatusFailed        = "FAILED"
	FileVersionStatusRemoved       = "REMOVED"
)

// 任务状态机：PENDING/RUNNING 为活动态，FAILED 可自动重试，DEAD/CANCELLED 为终态。
const (
	JobStatusPending   = "PENDING"
	JobStatusRunning   = "RUNNING"
	JobStatusSucceeded = "SUCCEEDED"
	JobStatusFailed    = "FAILED"
	JobStatusDead      = "DEAD"
	JobStatusCancelled = "CANCELLED"
)

// File 保存本地存储文件的逻辑元数据。有意不含存储路径：API 调用方绝不能收到
// 服务端相对路径。
type File struct {
	ID, TenantID, ApplicationID       string
	Namespace, Purpose, PolicyVersion string
	AuthenticatedClientID             string
	RetentionClass                    string
	RetentionUntil                    *time.Time
	OriginalName, FileExtension       string
	MediaType, Classification         string
	OwnerUserID                       string
	CurrentVersionNo                  uint
	CurrentVersionID                  string
	Status                            string
	Version                           uint64
	CreatedAt, UpdatedAt              time.Time
}

// FileVersion 是一个不可变的二进制版本。StorageRelativePath 仅限内部使用，
// HTTP 适配器不得返回。
type FileVersion struct {
	ID, FileID, StorageRelativePath string
	VersionNo                       uint
	SizeBytes                       uint64
	SHA256                          []byte
	MediaType, OriginalName, Status string
	UploaderUserID, UploadRequestID string
	UploadRequestHash               []byte
	CreatedAt                       time.Time
	ValidatedAt                     *time.Time
}

// StoredFile 把文件与其当前二进制版本组合，供安全下载与清理使用。
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

// PageRequest 是共用的、租户隔离的操作查询形态。
type PageRequest struct {
	Page, PageSize       int
	Status, JobType      string
	ApplicationID, Query string
}

// PageResult 是确定性的、便于分页的结果集。
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// Job 表示一个基于 MySQL 的异步任务。调度器有意不解释 Payload：
// 只有注册到对应 JobType 的 worker 才能解析它。
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

// CleanupResult 描述一次有界的本地文件清理。有意只暴露计数，不暴露物理路径或内容。
type CleanupResult struct {
	ClaimedFiles       int `json:"claimed_files"`
	DeletedFiles       int `json:"deleted_files"`
	FailedFiles        int `json:"failed_files"`
	RemovedTempFiles   int `json:"removed_temp_files"`
	TempCleanupFailure int `json:"temp_cleanup_failure"`
	// ReconciledFiles 统计通过受控对账推进的遗留 DELETING 记录（新增字段，既有字段语义不变）。
	ReconciledFiles int `json:"reconciled_files"`
}

// UploadSessionRecovery 描述与卡住文件一起恢复所需的上传会话字段。
//
// V2 上传把文件对象与上传会话分别落库：文件被对账推进到 READY 时，会话若仍停在
// UPLOADING，客户端会永久轮询不到结果，业务绑定也不会发生。恢复必须同时收敛会话。
type UploadSessionRecovery struct {
	SessionID, TenantID, ApplicationID, ActorUserID string
	FileID, ResourceType, ResourceID, BindingType   string
	DisplayName, OriginalName                       string
}

// ReconcileResult 汇总一次有界文件状态对账，不暴露物理路径或内容。
type ReconcileResult struct {
	Inspected int `json:"inspected"`
	Recovered int `json:"recovered"`
	Rejected  int `json:"rejected"`
	Failed    int `json:"failed"`
	Conflicts int `json:"conflicts"`
}
