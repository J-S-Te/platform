// Package application 协调通知模板、受众解析与站内信用例。
package application

import (
	"context"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// IdentifierGenerator 为通知聚合提供可排序的 ULID。
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock 使通知时间戳在测试中可确定。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境的 UTC 时钟。
type SystemClock struct{}

// Now 返回当前 UTC 墙钟时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// DeliveryVisibilityPolicy 是读取租户通知设置的有意收窄契约，仅在创建时刻调用。
// 通知模块不拥有、不复制也不导入 settings 聚合：适配器只返回决策（站内信是否启用）
// 以及新投递对收件人可见的时刻。
type DeliveryVisibilityPolicy interface {
	DeliveryVisibility(ctx context.Context, tenantID string, now time.Time) (enabled bool, remindAt time.Time, err error)
}

// RecipientResolver 把业务受众目标解析为在册用户 ID。实现必须保持租户隔离，
// 且不得返回已停用用户或已失效的任职关系。
type RecipientResolver interface {
	ResolveRecipients(ctx context.Context, tenantID string, targets []domain.RecipientTarget, at time.Time) ([]string, error)
}

// NotificationCreator 是面向审计告警等模块的公开应用契约。调用方只提供模板编码与
// 受众引用，绝不直接写通知表。
type NotificationCreator interface {
	Create(ctx context.Context, input CreateInput) (CreateResult, error)
}

// PageRequest 是有界分页的站内信/模板列表查询参数。
type PageRequest struct {
	Page       int
	PageSize   int
	UnreadOnly bool
}

// PageResult 是通知查询共用的分页结果结构。
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// CreateTemplateInput 创建启用或停用的模板及其首个不可变版本。
type CreateTemplateInput struct {
	TenantID      string
	OperatorID    string
	Code          string
	Name          string
	Status        domain.TemplateStatus
	TitleTemplate string
	BodyTemplate  string
	Variables     []domain.VariableDefinition
}

// CreateTemplateVersionInput 为模板追加并发布一个新的不可变版本。
type CreateTemplateVersionInput struct {
	TenantID      string
	OperatorID    string
	TemplateID    string
	TitleTemplate string
	BodyTemplate  string
	Variables     []domain.VariableDefinition
}

// ChangeTemplateStatusInput 在乐观锁下启用或停用模板。
type ChangeTemplateStatusInput struct {
	TenantID   string
	OperatorID string
	TemplateID string
	Status     domain.TemplateStatus
	Version    uint64
}

// CreateInput 描述一个将被渲染为站内信的业务事件。变量值只接受纯文本并在入库前转义；
// TargetURL 必须是应用内相对路径——打开它时必须重新执行授权，不能依赖通知本身隐含的权限。
type CreateInput struct {
	TenantID       string
	OperatorID     string
	TemplateCode   string
	Category       string
	Variables      map[string]string
	Recipients     []domain.RecipientTarget
	TargetURL      string
	ReferenceType  string
	ReferenceID    string
	IdempotencyKey string
}

// CreateResult 报告持久化后的消息与收件投递 ID。Suppressed 为 true 表示租户既有
// 站内信开关处于关闭状态，此时不保存任何消息或投递。
type CreateResult struct {
	MessageID   string
	DeliveryIDs []string
	Replayed    bool
	Suppressed  bool
}

// IngestInput 是可信来源绑定加一条外部通知事件。租户/应用/环境取自已验证的
// 应用主体（由 HTTP 适配器填入），绝不承认事件正文里的同名字段为权威来源。
type IngestInput struct {
	TenantID, SourceApplication, SourceEnvironment string
	Event                                          domain.IngestionEvent
}

// RetryResult 报告一次有界重试的结果。站内信投递只是数据库状态迁移；
// 本契约有意不包含邮件、短信或 Webhook 行为。
type RetryResult struct {
	Claimed   int
	Delivered int
	Failed    int
}

// MessageCreation 是仓储幂等持久化消息后的结果；Replayed 表示命中既有消息。
type MessageCreation struct {
	Message    domain.Message
	Deliveries []domain.Delivery
	Replayed   bool
}

// Repository 拥有全部数据库变更。凡涉及多行写入的方法都必须由基础设施适配器
// 包裹在 GORM 事务内执行，应用层不自行组事务。
type Repository interface {
	CreateTemplate(ctx context.Context, template domain.Template, version domain.TemplateVersion) (domain.Template, domain.TemplateVersion, error)
	AppendTemplateVersion(ctx context.Context, tenantID, templateID, operatorID string, versionID string, titleTemplate, bodyTemplate string, variables []domain.VariableDefinition, now time.Time) (domain.Template, domain.TemplateVersion, error)
	ChangeTemplateStatus(ctx context.Context, input ChangeTemplateStatusInput, now time.Time) (domain.Template, error)
	ListTemplates(ctx context.Context, tenantID string, page PageRequest) (PageResult[domain.Template], error)
	GetActiveTemplateByCode(ctx context.Context, tenantID, code string) (domain.Template, domain.TemplateVersion, error)

	CreateMessage(ctx context.Context, message domain.Message, deliveries []domain.Delivery) (MessageCreation, error)
	CompleteDelivery(ctx context.Context, tenantID, deliveryID string, now time.Time) (domain.Delivery, error)
	FailDelivery(ctx context.Context, tenantID, deliveryID, safeReason string, nextRetryAt time.Time, now time.Time) error
	ClaimFailedDeliveries(ctx context.Context, tenantID string, limit int, leaseUntil, now time.Time) ([]domain.Delivery, error)
	// ClaimDueDeliveries 仅供后台任务跨租户领取；HTTP 运维路径继续使用带租户的领取方法。
	ClaimDueDeliveries(ctx context.Context, limit int, leaseUntil, now time.Time) ([]domain.Delivery, error)
	ListDeliveries(ctx context.Context, tenantID string, status domain.DeliveryStatus, page PageRequest) (PageResult[domain.Delivery], error)

	ListInbox(ctx context.Context, tenantID, userID string, page PageRequest, now time.Time) (PageResult[domain.InboxItem], error)
	GetInboxItem(ctx context.Context, tenantID, userID, deliveryID string, now time.Time) (domain.InboxItem, error)
	CountUnread(ctx context.Context, tenantID, userID string, now time.Time) (int64, error)
	MarkRead(ctx context.Context, tenantID, userID, deliveryID string, now time.Time) (domain.InboxItem, error)
	MarkAllRead(ctx context.Context, tenantID, userID string, now time.Time) (int64, error)

	AcceptIngestion(ctx context.Context, tenantID, sourceApplication, sourceEnvironment string, event domain.IngestionEvent, receiptID string, now time.Time) (domain.IngestionReceipt, error)
	GetIngestionReceipt(ctx context.Context, tenantID, sourceApplication, sourceEnvironment, receiptID string) (domain.IngestionReceipt, error)
	ClaimIngestionEvents(ctx context.Context, limit int, leaseUntil, now time.Time) ([]domain.IngestionReceipt, error)
	LoadIngestionEvent(ctx context.Context, receiptID string) (string, string, string, domain.IngestionEvent, error)
	CompleteIngestion(ctx context.Context, receiptID, messageID string, now time.Time) error
	RetryIngestion(ctx context.Context, receiptID, errorCode, safeMessage string, nextRetryAt, now time.Time) error
	DeadLetterIngestion(ctx context.Context, receiptID, errorCode, safeMessage string, now time.Time) error
}
