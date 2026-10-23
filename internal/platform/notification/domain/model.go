// Package domain 定义租户隔离的站内信通知聚合。
package domain

import "time"

// TemplateStatus 控制模板当前发布版本能否用于创建消息。
type TemplateStatus string

const (
	// TemplateStatusActive 放行当前发布版本创建消息。
	TemplateStatusActive TemplateStatus = "ACTIVE"
	// TemplateStatusDisabled 冻结创建入口但不删除既有消息。
	TemplateStatusDisabled TemplateStatus = "DISABLED"
)

// TemplateVersionStatus 描述模板版本的不可变生命周期。
type TemplateVersionStatus string

const (
	// TemplateVersionPublished 标记已发布的不可变版本；同一模板同时只有一个发布版本。
	TemplateVersionPublished TemplateVersionStatus = "PUBLISHED"
)

// RecipientType 声明预期通知受众的解析方式。
type RecipientType string

const (
	// RecipientTypeUser 按用户 ID 投递。
	RecipientTypeUser RecipientType = "USER"
	// RecipientTypeRole 按应用角色展开为用户集合投递。
	RecipientTypeRole RecipientType = "ROLE"
	// RecipientTypeOrganization 按组织展开为成员集合投递。
	RecipientTypeOrganization RecipientType = "ORGANIZATION"
)

// DeliveryStatus 记录站内信投递的生命周期。
type DeliveryStatus string

const (
	// DeliveryStatusPending 已创建待投递。
	DeliveryStatusPending DeliveryStatus = "PENDING"
	// DeliveryStatusProcessing 已被 worker 认领处理中。
	DeliveryStatusProcessing DeliveryStatus = "PROCESSING"
	// DeliveryStatusDelivered 已写入收件人收件箱。
	DeliveryStatusDelivered DeliveryStatus = "DELIVERED"
	// DeliveryStatusFailed 表示投递失败；持久化的 next_retry_at 到期后允许后台或人工重试。
	DeliveryStatusFailed DeliveryStatus = "FAILED"
)

// IngestionStatus 是外部提交通知事件的异步生命周期。ACCEPTED 只表示已持久化接收；
// COMPLETED 才表示事件已展开为站内信投递。区分二者防止调用方把快速的 HTTP 确认
// 误认为对用户可见的投递保证。
type IngestionStatus string

const (
	// IngestionStatusAccepted 已持久化接收，尚未处理。
	IngestionStatusAccepted IngestionStatus = "ACCEPTED"
	// IngestionStatusProcessing 已被 worker 认领。
	IngestionStatusProcessing IngestionStatus = "PROCESSING"
	// IngestionStatusCompleted 事件已展开为站内信投递。
	IngestionStatusCompleted IngestionStatus = "COMPLETED"
	// IngestionStatusRetry 处理失败待重试。
	IngestionStatusRetry IngestionStatus = "RETRY"
	// IngestionStatusDead 重试耗尽的终态。
	IngestionStatusDead IngestionStatus = "DEAD"
	// IngestionStatusCancelled 上游显式取消。
	IngestionStatusCancelled IngestionStatus = "CANCELLED"
)

// VariableDefinition 声明允许的纯文本模板变量。变量值在持久化前转义，
// 客户端必须把通知内容当纯文本渲染，而不是可执行 HTML。
type VariableDefinition struct {
	Name      string `json:"name"`
	Required  bool   `json:"required"`
	MaxLength int    `json:"max_length"`
}

// Template 是可变的模板聚合；其已发布版本保持不可变。
type Template struct {
	ID             string
	TenantID       string
	Code           string
	Name           string
	Status         TemplateStatus
	CurrentVersion uint64
	Version        uint64
	CreatedAt      time.Time
	CreatedBy      string
	UpdatedAt      time.Time
	UpdatedBy      string
}

// TemplateVersion 保存一份不可变的已发布渲染定义。
type TemplateVersion struct {
	ID            string
	TemplateID    string
	TenantID      string
	Version       uint64
	Status        TemplateVersionStatus
	TitleTemplate string
	BodyTemplate  string
	Variables     []VariableDefinition
	PublishedAt   time.Time
	CreatedAt     time.Time
	CreatedBy     string
}

// RecipientTarget 是面向业务的受众引用。它在通知持久化前被解析为在册用户；
// 通知记录本身绝不授予其关联资源的访问权。
type RecipientTarget struct {
	Type RecipientType
	ID   string
}

// Message 是渲染后的业务通知，可被多个用户投递共享。
type Message struct {
	ID                string
	TenantID          string
	SourceApplication string
	SourceEnvironment string
	SourceEventID     string
	EventType         string
	NotificationScope string
	Priority          string
	TemplateID        string
	TemplateVersionID string
	Category          string
	Title             string
	Content           string
	TargetURL         string
	ReferenceType     string
	ReferenceID       string
	IdempotencyKey    string
	OccurredAt        *time.Time
	ExpiresAt         *time.Time
	CreatedAt         time.Time
	CreatedBy         string
}

// IngestionEvent 是经应用认证、已渲染完成的通知请求。它有意只接受具体的 USER
// 目标：外部系统不得把平台当作业务角色解析器使用。
type IngestionEvent struct {
	EventID, EventType, NotificationScope, Priority string
	Title, Content, TargetURL                       string
	ReferenceType, ReferenceID, IdempotencyKey      string
	Recipients                                      []string
	OccurredAt                                      time.Time
	ExpiresAt                                       *time.Time
}

// IngestionReceipt 可被来源系统安全保留，用于重试与对账。
type IngestionReceipt struct {
	ReceiptID, EventID, MessageID string
	Status                        IngestionStatus
	Duplicate                     bool
	ErrorCode                     string
	ReceivedAt, ProcessedAt       time.Time
}

// Delivery 是单个收件人的站内信条目及其投递重试状态。
type Delivery struct {
	ID              string
	TenantID        string
	MessageID       string
	RecipientUserID string
	Status          DeliveryStatus
	AttemptCount    uint
	LastError       string
	NextRetryAt     *time.Time
	LockedUntil     *time.Time
	DeliveredAt     *time.Time
	ReadAt          *time.Time
	// RemindAt 是收件人可见时间：NULL/过去表示立即可见；未来值表示按租户提醒频率
	// 延迟浮现（DAILY/WEEKLY 汇总语义）。站内信查询与未读计数都以它过滤。
	RemindAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InboxItem 是对收件人安全的站内信投影；客户端跳转关联目标时须重新授权。
type InboxItem struct {
	DeliveryID    string     `json:"delivery_id"`
	MessageID     string     `json:"message_id"`
	Category      string     `json:"category"`
	Title         string     `json:"title"`
	Content       string     `json:"content"`
	TargetURL     string     `json:"target_url"`
	ReferenceType string     `json:"reference_type"`
	ReferenceID   string     `json:"reference_id"`
	DeliveredAt   time.Time  `json:"delivered_at"`
	ReadAt        *time.Time `json:"read_at"`
}
