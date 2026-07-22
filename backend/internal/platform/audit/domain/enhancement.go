package domain

import "time"

const (
	// RetentionTaskArchive 将在线审计记录写入受控归档文件，不修改 audit_event 原始记录。
	RetentionTaskArchive = "ARCHIVE"
	// RetentionTaskPurge 仅删除已由归档清单证明已归档的在线审计记录。
	RetentionTaskPurge = "PURGE"

	RetentionTaskPending   = "PENDING"
	RetentionTaskRunning   = "RUNNING"
	RetentionTaskCompleted = "COMPLETED"
	RetentionTaskFailed    = "FAILED"

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

// IngestionReceipt describes one externally submitted batch delivery. It records receiver-side
// transport correlation independently from each audit event's original business correlation.
type IngestionReceipt struct {
	ID                                                                                        uint64
	TenantID, ApplicationID, ApplicationCode, ApplicationName, EnvironmentID, EnvironmentCode string
	ClientID, RequestID, TraceID, CorrelationID                                               string
	EventCount, AcceptedCount, DuplicateCount                                                 uint
	Status, SourceIP, UserAgent                                                               string
	ReceivedAt, CreatedAt                                                                     time.Time
}

// DeadLetterReplayResult records the observable outcome of one manual replay attempt. It never
// contains the stored payload, credentials, or raw upstream transport errors.
type DeadLetterReplayResult struct {
	DeadLetterID, EventID, Status, ReceiptStatus, ErrorCode string
	ReplayedAt                                              time.Time
}
