package domain

import "time"

const (
	// RetentionTaskArchive 将在线审计记录写入受控归档文件，不修改 audit_event 原始记录。
	RetentionTaskArchive = "ARCHIVE"
	// RetentionTaskPurge 仅删除已由归档清单证明已归档的在线审计记录。
	RetentionTaskPurge = "PURGE"

	// 任务状态机：任务只能单向推进；FAILED 任务必须由人工重新发起，不自动重试。
	RetentionTaskPending   = "PENDING"
	RetentionTaskRunning   = "RUNNING"
	RetentionTaskCompleted = "COMPLETED"
	RetentionTaskFailed    = "FAILED"

	// 死信状态：PENDING 可重放或忽略，REPLAYED/IGNORED 均为终态。
	DeadLetterPending  = "PENDING"
	DeadLetterReplayed = "REPLAYED"
	DeadLetterIgnored  = "IGNORED"
)

// RetentionTask 是受控的审计归档或清理申请。模块不提供按事件直接删除的领域操作。
type RetentionTask struct {
	TaskID, TenantID, ApplicationID, RequestedBy, Mode, Status, ArchiveID string
	CutoffAt                                                              time.Time
	CandidateCount, ProcessedCount                                        uint64
	FailureCode, FailureMessage                                           string
	CreatedAt, StartedAt, CompletedAt                                     time.Time
	// WorkerID 是领取该任务时写入 worker_id 的执行者标识。完成/失败回写必须带它，
	// 否则租约过期被接管后，旧执行者仍能覆盖接管者的结果。
	WorkerID string
}

// Archive 描述只读归档文件及其完整性摘要。物理文件只允许由受控 worker 创建。
type Archive struct {
	ArchiveID, TenantID, ApplicationID, StorageRelativePath, MediaType string
	SHA256                                                             []byte
	EventCount                                                         uint64
	OccurredFrom, OccurredTo, CreatedAt                                time.Time
}

// ArchiveItem 是在线事件删除前保留的最小归档清单，不保存重复的业务正文。
type ArchiveItem struct {
	ArchiveID     string
	AuditRowID    uint64
	OccurredMonth uint
}

// DeadLetter 保存未能被接收或人工重放的审计上报。Payload 已由应用层做脱敏。
type DeadLetter struct {
	DeadLetterID, TenantID, ApplicationCode, EnvironmentCode, EventID, Status string
	Payload, LastErrorCode, LastErrorMessage                                  []byte
	Attempts                                                                  uint
	CreatedAt, UpdatedAt, ReplayedAt                                          time.Time
}

// DeadLetterStatus 是运营页展示的、按状态聚合后的死信状态。
type DeadLetterStatus struct {
	TenantID, ApplicationCode  string
	Pending, Replayed, Ignored uint64
	OldestPendingAt            *time.Time
}

// IngestionReceipt 描述一次外部提交的批量投递。它按接收侧记录传输层关联信息，
// 与每条审计事件原有的业务关联字段相互独立。
type IngestionReceipt struct {
	ID                                                                                        uint64
	TenantID, ApplicationID, ApplicationCode, ApplicationName, EnvironmentID, EnvironmentCode string
	ClientID, RequestID, TraceID, CorrelationID                                               string
	EventCount, AcceptedCount, DuplicateCount                                                 uint
	Status, SourceIP, UserAgent                                                               string
	ReceivedAt, CreatedAt                                                                     time.Time
}

// DeadLetterReplayResult 记录一次手动重放的可观测结果。它绝不包含已存储的 payload、
// 凭据或上游原始传输错误。
type DeadLetterReplayResult struct {
	DeadLetterID, EventID, Status, ReceiptStatus, ErrorCode string
	ReplayedAt                                              time.Time
}
