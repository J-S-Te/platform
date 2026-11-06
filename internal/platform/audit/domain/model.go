// Package domain 定义可安全通过审计控制台展示的审计聚合。
package domain

import "time"

// Event 是只追加的审计记录，供已授权的审计控制台读取。包含关联字段，
// 使管理员可以跨服务追踪同一事件。
type Event struct {
	ID, EventID, TenantID, ApplicationID, ApplicationCode, ApplicationName string
	EnvironmentID, EnvironmentCode                                         string
	OccurredAt                                                             time.Time
	OperatorDisplayName, ActionType, Action, Result                        string
	ResourceType, ResourceID, ResourceName                                 string
	Method, Path, ClientIP, UserAgent, RiskLevel, Detail, Summary          string
	// 三类来源地址互相独立：登录地址在会话创建时固定，操作地址每个请求重新解析，
	// 投递地址只描述"谁把审计送来"。IPSource 标记地址的获取方式，历史数据为 LEGACY_RECORD。
	LoginIP, IngestPeerIP, IPSource   string
	RequestID, TraceID, CorrelationID string
	StatusCode                        int
	ChangeSummary                     []FieldChange
	// 稳定定位字段（统一审计方案 §2.2）：名称只是快照，追溯必须依赖 ID。
	ActorID    string
	ActorType  string
	ReasonCode string
	BusinessID string
	ReceivedAt time.Time
	// LoginIPSource 记录登录地址的获取方式；历史记录为空，不回填、不推算。
	LoginIPSource string
}

// FieldChange 保存脱敏后的变更摘要；绝不能包含凭据材料。
type FieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// Receipt 报告摄取事件是本次接收还是此前已去重。
type Receipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// PayloadHashVerification 是对单条审计事件完整性摘要的重算比对结果（审计 P0 · 防篡改）。
// Algorithm 取自存储行；ComputedHash 由候选规范输入按同一算法（HMAC 需平台密钥）重算。
type PayloadHashVerification struct {
	EventID      string
	Algorithm    string
	Matched      bool
	StoredHash   []byte
	ComputedHash []byte
}

// ChainVerification 是哈希链分段校验结果（审计 P1 · 防篡改）。
// 每行自证：H(chain_prev_hash, payload_hash) == chain_hash（HMAC 需平台密钥）；
// 窗口内相邻行做序列连续性与链指针连续性校验。链首（sequence=1）额外要求 prev 为全零。
type ChainVerification struct {
	TenantID             string
	ApplicationCode      string
	EnvironmentCode      string
	FromSequence         uint64
	CheckedRows          int
	LastSequence         uint64
	Verified             bool
	MoreRemaining        bool
	BrokenSequence       uint64
	BrokenEventID        string
	Reason               string
}

// ExportJob 表示一次异步处理的审计导出。
type ExportJob struct {
	JobID, Status, DownloadURL string
	ErrorCode, ErrorMessage    string
	CreatedAt, CompletedAt     time.Time
}

// ExportWork 是被单个本地 worker 进程领取的审计导出任务。
type ExportWork struct {
	JobID, TenantID, ApplicationID, OperatorID string
	Query                                      ExportQuery
	Attempts, MaxAttempts                      uint
	// LockedBy 是领取该任务时写入 locked_by 的 Worker 标识。完成/失败回写必须带它，
	// 否则租约过期被他人接管后，原 Worker 仍能覆盖接管者的结果。
	LockedBy string
}

// ExportQuery 是生成审计导出时持久化的过滤条件集合。
type ExportQuery struct {
	Keyword, ApplicationCode, EnvironmentCode, Action, ActionCategory, Result, RiskLevel string
	OccurredFrom, OccurredTo                                                             *time.Time
	// 与列表共用同一组精确过滤条件，避免“页面过滤正确、导出越界”。
	EventID, ActorID, CorrelationID, ClientIP, LoginIP, ResourceID, RequestID string
}

// ExportCursor 是审计导出分批读取的稳定游标：上一批最后一行的（occurred_at, id）。
// 零值表示从头读取；排序固定为 occurred_at DESC, id DESC，因此同一快照内不会重复或遗漏。
type ExportCursor struct {
	OccurredAt time.Time
	ID         uint64
}

// ExportEventPage 是导出分页读取的一批事件。NextCursor 为 nil 表示已读完；
// 仓储多取一行来判断是否还有后续批次，因此调用方无需额外查询。
type ExportEventPage struct {
	Events     []Event
	NextCursor *ExportCursor
}

// ExportArtifact 是导出任务产物在存储层的定位信息，供保留期清理使用。
// 只暴露 file_version 标识与相对路径，不暴露租户或其他模块的文件。
type ExportArtifact struct {
	FileVersionID       string
	StorageRelativePath string
}

// ExportFile 描述已完成的审计导出任务附带的本地文件。
// Status 取自 file_version.status：DELETED/REMOVED 表示产物已过期或已被清理。
type ExportFile struct {
	FileID, VersionID, StorageRelativePath, OriginalName, MediaType string
	Status                                                          string
	SizeBytes                                                       uint64
	SHA256                                                          []byte
	CreatedAt                                                       time.Time
}

// ExportDownloadTicket 是一次性导出下载票据的服务端记录（审计导出下载契约变更）。
// TokenHash 是明文 token 的 SHA-256 摘要：明文只在签发响应里出现一次，绝不进入本结构、
// 日志或审计载荷，因此本表泄露也无法兑换文件。
type ExportDownloadTicket struct {
	ID, TenantID, JobID string
	TokenHash           []byte
	ExpiresAt           time.Time
	UsedAt              *time.Time
	CreatedAt           time.Time
}

// ExportDownloadTicketState 是票据原子核销的结果。
type ExportDownloadTicketState string

const (
	// ExportDownloadTicketConsumed 表示本次调用成功核销票据，可以输出文件。
	ExportDownloadTicketConsumed ExportDownloadTicketState = "CONSUMED"
	// ExportDownloadTicketInvalid 表示票据不存在、不属于该租户/任务，或已被使用。
	ExportDownloadTicketInvalid ExportDownloadTicketState = "INVALID"
	// ExportDownloadTicketExpired 表示票据存在且未使用，但已超过有效期。
	ExportDownloadTicketExpired ExportDownloadTicketState = "EXPIRED"
)
