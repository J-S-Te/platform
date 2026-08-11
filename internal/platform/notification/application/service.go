package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

var (
	// ErrNotFound 统一隐藏通知不存在、属于其他租户或属于其他收件人的差异。
	ErrNotFound = errors.New("notification resource not found")
	// ErrConflict 表示幂等键或状态转换冲突，不暴露已存在消息的正文。
	ErrConflict = errors.New("notification resource conflict")
	// ErrVersionConflict identifies a stale template status update.
	ErrVersionConflict = errors.New("notification template version conflict")
	// ErrValidation identifies invalid input without disclosing message content.
	ErrValidation = errors.New("notification validation failed")
	// ErrNoRecipients indicates no active user resulted from the supplied audience references.
	ErrNoRecipients = errors.New("notification recipients are empty")
)

const (
	defaultPageSize  = 20
	maximumPageSize  = 100
	maximumRetrySize = 100
	retryLease       = 2 * time.Minute
)

// Service exposes the notification application's tenant-scoped use cases.
type Service struct {
	repository Repository
	policy     InboxPolicy
	resolver   RecipientResolver
	ids        IdentifierGenerator
	clock      Clock
}

// NewService validates and constructs the notification application service.
func NewService(repository Repository, policy InboxPolicy, resolver RecipientResolver, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || policy == nil || resolver == nil || ids == nil || clock == nil {
		return nil, errors.New("notification service dependencies must not be nil")
	}
	return &Service{repository: repository, policy: policy, resolver: resolver, ids: ids, clock: clock}, nil
}

// CreateTemplate 在同一事务中建立模板和首个不可变已发布版本；后续通知始终引用具体版本，
// 模板演进不会改写历史消息的渲染依据。
func (service *Service) CreateTemplate(ctx context.Context, input CreateTemplateInput) (domain.Template, domain.TemplateVersion, error) {
	if err := validateTemplateInput(input.TenantID, input.OperatorID, input.Code, input.Name, input.Status, input.TitleTemplate, input.BodyTemplate, input.Variables); err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	now := service.clock.Now().UTC()
	templateID, err := service.ids.New(now)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, fmt.Errorf("generate notification template ID: %w", err)
	}
	versionID, err := service.ids.New(now)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, fmt.Errorf("generate notification template version ID: %w", err)
	}

	template := domain.Template{ID: templateID, TenantID: strings.TrimSpace(input.TenantID), Code: normalizeCode(input.Code), Name: strings.TrimSpace(input.Name), Status: input.Status, CurrentVersion: 1, Version: 1, CreatedAt: now, CreatedBy: strings.TrimSpace(input.OperatorID), UpdatedAt: now, UpdatedBy: strings.TrimSpace(input.OperatorID)}
	version := domain.TemplateVersion{ID: versionID, TemplateID: templateID, TenantID: template.TenantID, Version: 1, Status: domain.TemplateVersionPublished, TitleTemplate: input.TitleTemplate, BodyTemplate: input.BodyTemplate, Variables: cloneVariables(input.Variables), PublishedAt: now, CreatedAt: now, CreatedBy: template.CreatedBy}
	return service.repository.CreateTemplate(ctx, template, version)
}

// CreateTemplateVersion 追加不可变版本并原子切换 current_version，防止并发创建得到重复版本号。
func (service *Service) CreateTemplateVersion(ctx context.Context, input CreateTemplateVersionInput) (domain.Template, domain.TemplateVersion, error) {
	if err := validateTemplateVersionInput(input); err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	now := service.clock.Now().UTC()
	versionID, err := service.ids.New(now)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, fmt.Errorf("generate notification template version ID: %w", err)
	}
	return service.repository.AppendTemplateVersion(ctx, strings.TrimSpace(input.TenantID), strings.TrimSpace(input.TemplateID), strings.TrimSpace(input.OperatorID), versionID, input.TitleTemplate, input.BodyTemplate, cloneVariables(input.Variables), now)
}

// ChangeTemplateStatus 只影响未来消息创建，已经渲染并持久化的收件箱内容不随模板禁用而消失。
func (service *Service) ChangeTemplateStatus(ctx context.Context, input ChangeTemplateStatusInput) (domain.Template, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.TemplateID) == "" || input.Version == 0 || !validTemplateStatus(input.Status) {
		return domain.Template{}, ErrValidation
	}
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.TemplateID = strings.TrimSpace(input.TemplateID)
	return service.repository.ChangeTemplateStatus(ctx, input, service.clock.Now().UTC())
}

// ListTemplates returns a bounded tenant-scoped template list.
func (service *Service) ListTemplates(ctx context.Context, tenantID string, page PageRequest) (PageResult[domain.Template], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Template]{}, ErrValidation
	}
	return service.repository.ListTemplates(ctx, strings.TrimSpace(tenantID), normalizePage(page))
}

// Create 先读取租户站内信开关，再用当前发布模板渲染并解析有效收件人，最后按幂等键持久化。
// 开关关闭时不写任何消息或投递记录；重复请求返回原投递集合，避免业务重试产生重复提醒。
func (service *Service) Create(ctx context.Context, input CreateInput) (CreateResult, error) {
	if err := validateCreateInput(input); err != nil {
		return CreateResult{}, err
	}
	input.TenantID = strings.TrimSpace(input.TenantID)
	enabled, err := service.policy.InboxEnabled(ctx, input.TenantID)
	if err != nil {
		return CreateResult{}, fmt.Errorf("read inbox policy: %w", err)
	}
	if !enabled {
		return CreateResult{Suppressed: true}, nil
	}

	now := service.clock.Now().UTC()
	template, templateVersion, err := service.repository.GetActiveTemplateByCode(ctx, input.TenantID, normalizeCode(input.TemplateCode))
	if err != nil {
		return CreateResult{}, err
	}
	title, err := renderTemplate(templateVersion.TitleTemplate, templateVersion.Variables, input.Variables, 500)
	if err != nil {
		return CreateResult{}, err
	}
	content, err := renderTemplate(templateVersion.BodyTemplate, templateVersion.Variables, input.Variables, 16*1024)
	if err != nil {
		return CreateResult{}, err
	}
	users, err := service.resolver.ResolveRecipients(ctx, input.TenantID, normalizeRecipients(input.Recipients), now)
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve notification recipients: %w", err)
	}
	users = uniqueNonEmpty(users)
	if len(users) == 0 {
		return CreateResult{}, ErrNoRecipients
	}

	messageID, err := service.ids.New(now)
	if err != nil {
		return CreateResult{}, fmt.Errorf("generate notification message ID: %w", err)
	}
	message := domain.Message{ID: messageID, TenantID: input.TenantID, TemplateID: template.ID, TemplateVersionID: templateVersion.ID, Category: normalizeCode(input.Category), Title: title, Content: content, TargetURL: strings.TrimSpace(input.TargetURL), ReferenceType: normalizeCode(input.ReferenceType), ReferenceID: strings.TrimSpace(input.ReferenceID), IdempotencyKey: strings.TrimSpace(input.IdempotencyKey), CreatedAt: now, CreatedBy: strings.TrimSpace(input.OperatorID)}
	deliveries := make([]domain.Delivery, 0, len(users))
	for _, userID := range users {
		deliveryID, idErr := service.ids.New(now)
		if idErr != nil {
			return CreateResult{}, fmt.Errorf("generate notification delivery ID: %w", idErr)
		}
		deliveries = append(deliveries, domain.Delivery{ID: deliveryID, TenantID: input.TenantID, MessageID: messageID, RecipientUserID: userID, Status: domain.DeliveryStatusPending, CreatedAt: now, UpdatedAt: now})
	}
	created, err := service.repository.CreateMessage(ctx, message, deliveries)
	if err != nil {
		return CreateResult{}, err
	}
	result := CreateResult{MessageID: created.Message.ID, DeliveryIDs: deliveryIDs(created.Deliveries), Replayed: created.Replayed}
	if created.Replayed {
		// 幂等重放已存在完整投递状态，不能再次执行 CompleteDelivery，否则会重复推进或制造冲突。
		return result, nil
	}
	for _, delivery := range created.Deliveries {
		if _, deliverErr := service.repository.CompleteDelivery(ctx, input.TenantID, delivery.ID, now); deliverErr != nil {
			if failErr := service.repository.FailDelivery(ctx, input.TenantID, delivery.ID, "站内信状态更新失败", retryAt(now, delivery.AttemptCount+1), now); failErr != nil {
				return result, fmt.Errorf("mark notification delivery failed: %w", failErr)
			}
		}
	}
	return result, nil
}

// RetryFailedDeliveries 领取有限数量的到期失败投递。领取动作把记录置为带租约的 PROCESSING，
// 多实例不会同时处理同一收件人；租约过期后才允许其他实例接管。
func (service *Service) RetryFailedDeliveries(ctx context.Context, tenantID string, limit int) (RetryResult, error) {
	if strings.TrimSpace(tenantID) == "" || limit < 1 {
		return RetryResult{}, ErrValidation
	}
	if limit > maximumRetrySize {
		limit = maximumRetrySize
	}
	now := service.clock.Now().UTC()
	deliveries, err := service.repository.ClaimFailedDeliveries(ctx, strings.TrimSpace(tenantID), limit, now.Add(retryLease), now)
	if err != nil {
		return RetryResult{}, err
	}
	result := RetryResult{Claimed: len(deliveries)}
	for _, delivery := range deliveries {
		if _, deliverErr := service.repository.CompleteDelivery(ctx, strings.TrimSpace(tenantID), delivery.ID, now); deliverErr != nil {
			if failErr := service.repository.FailDelivery(ctx, strings.TrimSpace(tenantID), delivery.ID, "站内信重试失败", retryAt(now, delivery.AttemptCount+1), now); failErr != nil {
				return result, fmt.Errorf("mark retried notification delivery failed: %w", failErr)
			}
			result.Failed++
			continue
		}
		result.Delivered++
	}
	return result, nil
}

// ListDeliveries returns a bounded tenant-scoped operations view. A status filter is optional.
func (service *Service) ListDeliveries(ctx context.Context, tenantID string, status domain.DeliveryStatus, page PageRequest) (PageResult[domain.Delivery], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.Delivery]{}, ErrValidation
	}
	if status != "" && status != domain.DeliveryStatusPending && status != domain.DeliveryStatusProcessing && status != domain.DeliveryStatusDelivered && status != domain.DeliveryStatusFailed {
		return PageResult[domain.Delivery]{}, ErrValidation
	}
	return service.repository.ListDeliveries(ctx, strings.TrimSpace(tenantID), status, normalizePage(page))
}

// ListInbox 只读取当前认证用户的 DELIVERED 项；运维投递列表与用户收件箱是两条不同的数据边界。
func (service *Service) ListInbox(ctx context.Context, tenantID, userID string, page PageRequest) (PageResult[domain.InboxItem], error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return PageResult[domain.InboxItem]{}, ErrValidation
	}
	return service.repository.ListInbox(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID), normalizePage(page))
}

// GetInboxItem reads exactly one delivered inbox item owned by the authenticated user.
func (service *Service) GetInboxItem(ctx context.Context, tenantID, userID, deliveryID string) (domain.InboxItem, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(deliveryID) == "" {
		return domain.InboxItem{}, ErrValidation
	}
	return service.repository.GetInboxItem(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID), strings.TrimSpace(deliveryID))
}

// CountUnread returns the authenticated recipient's delivered unread item count.
func (service *Service) CountUnread(ctx context.Context, tenantID, userID string) (int64, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return 0, ErrValidation
	}
	return service.repository.CountUnread(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID))
}

// MarkRead 只更新当前收件人拥有且已投递的记录；重复已读操作保持幂等，不泄露其他用户的投递 ID。
func (service *Service) MarkRead(ctx context.Context, tenantID, userID, deliveryID string) (domain.InboxItem, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(deliveryID) == "" {
		return domain.InboxItem{}, ErrValidation
	}
	return service.repository.MarkRead(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID), strings.TrimSpace(deliveryID), service.clock.Now().UTC())
}

// MarkAllRead marks all delivered inbox items belonging to one authenticated user as read.
func (service *Service) MarkAllRead(ctx context.Context, tenantID, userID string) (int64, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return 0, ErrValidation
	}
	return service.repository.MarkAllRead(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID), service.clock.Now().UTC())
}

func normalizePage(page PageRequest) PageRequest {
	if page.Page < 1 {
		page.Page = 1
	}
	if page.PageSize < 1 {
		page.PageSize = defaultPageSize
	}
	if page.PageSize > maximumPageSize {
		page.PageSize = maximumPageSize
	}
	return page
}

func deliveryIDs(deliveries []domain.Delivery) []string {
	ids := make([]string, 0, len(deliveries))
	for _, delivery := range deliveries {
		ids = append(ids, delivery.ID)
	}
	return ids
}

func retryAt(now time.Time, attempts uint) time.Time {
	shift := attempts
	if shift > 6 {
		shift = 6
	}
	return now.Add(time.Minute * time.Duration(1<<shift))
}
