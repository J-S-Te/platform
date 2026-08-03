// Package infrastructure 实现站内信模板、消息和投递状态的 MySQL/GORM 适配器。
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository persists notification templates, messages and inbox deliveries through GORM.
type Repository struct{ database *gorm.DB }

// NewRepository creates a notification repository backed by MySQL.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("notification database must not be nil")
	}
	return &Repository{database: database}, nil
}

type templateModel struct {
	ID             string    `gorm:"column:id;primaryKey"`
	TenantID       string    `gorm:"column:tenant_id"`
	Code           string    `gorm:"column:code"`
	Name           string    `gorm:"column:name"`
	Status         string    `gorm:"column:status"`
	CurrentVersion uint64    `gorm:"column:current_version"`
	Version        uint64    `gorm:"column:version"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	CreatedBy      *string   `gorm:"column:created_by"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
	UpdatedBy      *string   `gorm:"column:updated_by"`
}

func (templateModel) TableName() string { return "notification_template" }

type templateVersionModel struct {
	ID            string    `gorm:"column:id;primaryKey"`
	TemplateID    string    `gorm:"column:template_id"`
	TenantID      string    `gorm:"column:tenant_id"`
	Version       uint64    `gorm:"column:version"`
	Status        string    `gorm:"column:status"`
	TitleTemplate string    `gorm:"column:title_template"`
	BodyTemplate  string    `gorm:"column:body_template"`
	VariablesJSON []byte    `gorm:"column:variables_json"`
	PublishedAt   time.Time `gorm:"column:published_at"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
}

func (templateVersionModel) TableName() string { return "notification_template_version" }

type messageModel struct {
	ID                string    `gorm:"column:id;primaryKey"`
	TenantID          string    `gorm:"column:tenant_id"`
	TemplateID        string    `gorm:"column:template_id"`
	TemplateVersionID string    `gorm:"column:template_version_id"`
	Category          string    `gorm:"column:category"`
	Title             string    `gorm:"column:title"`
	Content           string    `gorm:"column:content"`
	TargetURL         string    `gorm:"column:target_url"`
	ReferenceType     string    `gorm:"column:reference_type"`
	ReferenceID       string    `gorm:"column:reference_id"`
	IdempotencyKey    string    `gorm:"column:idempotency_key"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	CreatedBy         *string   `gorm:"column:created_by"`
}

func (messageModel) TableName() string { return "notification_message" }

type deliveryModel struct {
	ID              string     `gorm:"column:id;primaryKey"`
	TenantID        string     `gorm:"column:tenant_id"`
	MessageID       string     `gorm:"column:message_id"`
	RecipientUserID string     `gorm:"column:recipient_user_id"`
	Status          string     `gorm:"column:status"`
	AttemptCount    uint       `gorm:"column:attempt_count"`
	LastError       string     `gorm:"column:last_error"`
	NextRetryAt     *time.Time `gorm:"column:next_retry_at"`
	LockedUntil     *time.Time `gorm:"column:locked_until"`
	DeliveredAt     *time.Time `gorm:"column:delivered_at"`
	ReadAt          *time.Time `gorm:"column:read_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (deliveryModel) TableName() string { return "notification_delivery" }

type inboxRow struct {
	DeliveryID    string     `gorm:"column:delivery_id"`
	MessageID     string     `gorm:"column:message_id"`
	Category      string     `gorm:"column:category"`
	Title         string     `gorm:"column:title"`
	Content       string     `gorm:"column:content"`
	TargetURL     string     `gorm:"column:target_url"`
	ReferenceType string     `gorm:"column:reference_type"`
	ReferenceID   string     `gorm:"column:reference_id"`
	DeliveredAt   time.Time  `gorm:"column:delivered_at"`
	ReadAt        *time.Time `gorm:"column:read_at"`
}

func (repository *Repository) CreateTemplate(ctx context.Context, template domain.Template, version domain.TemplateVersion) (domain.Template, domain.TemplateVersion, error) {
	variables, err := marshalVariables(version.Variables)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	createdBy, updatedBy := optional(template.CreatedBy), optional(template.UpdatedBy)
	versionCreatedBy := optional(version.CreatedBy)
	row := templateModel{ID: template.ID, TenantID: template.TenantID, Code: template.Code, Name: template.Name, Status: string(template.Status), CurrentVersion: template.CurrentVersion, Version: template.Version, CreatedAt: template.CreatedAt, CreatedBy: createdBy, UpdatedAt: template.UpdatedAt, UpdatedBy: updatedBy}
	versionRow := templateVersionModel{ID: version.ID, TemplateID: version.TemplateID, TenantID: version.TenantID, Version: version.Version, Status: string(version.Status), TitleTemplate: version.TitleTemplate, BodyTemplate: version.BodyTemplate, VariablesJSON: variables, PublishedAt: version.PublishedAt, CreatedAt: version.CreatedAt, CreatedBy: versionCreatedBy}
	if err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return mapError(err)
		}
		if err := tx.Create(&versionRow).Error; err != nil {
			return mapError(err)
		}
		return nil
	}); err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	return template, version, nil
}

func (repository *Repository) AppendTemplateVersion(ctx context.Context, tenantID, templateID, operatorID, versionID, titleTemplate, bodyTemplate string, variables []domain.VariableDefinition, now time.Time) (domain.Template, domain.TemplateVersion, error) {
	// 锁住模板聚合后计算下一版本并切换当前版本，保证并发发布不会生成相同 version_no。
	encoded, err := marshalVariables(variables)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	var resultTemplate domain.Template
	var resultVersion domain.TemplateVersion
	err = repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row templateModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ?", templateID, tenantID).Take(&row).Error; err != nil {
			return mapError(err)
		}
		next := row.CurrentVersion + 1
		createdBy := operatorID
		versionRow := templateVersionModel{ID: versionID, TemplateID: templateID, TenantID: tenantID, Version: next, Status: string(domain.TemplateVersionPublished), TitleTemplate: titleTemplate, BodyTemplate: bodyTemplate, VariablesJSON: encoded, PublishedAt: now, CreatedAt: now, CreatedBy: &createdBy}
		if err := tx.Create(&versionRow).Error; err != nil {
			return mapError(err)
		}
		row.CurrentVersion = next
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := tx.Model(&templateModel{}).Where("id = ? AND tenant_id = ?", templateID, tenantID).Select("current_version", "version", "updated_at", "updated_by").Updates(&row).Error; err != nil {
			return fmt.Errorf("update notification template version: %w", err)
		}
		resultTemplate = templateToDomain(row)
		resultVersion, err = templateVersionToDomain(versionRow)
		return err
	})
	return resultTemplate, resultVersion, err
}

func (repository *Repository) ChangeTemplateStatus(ctx context.Context, input application.ChangeTemplateStatusInput, now time.Time) (domain.Template, error) {
	// 行锁与 Version 双重约束把旧管理页面的覆盖写转换为明确的版本冲突。
	var row templateModel
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ?", input.TemplateID, input.TenantID).Take(&row).Error; err != nil {
			return mapError(err)
		}
		if row.Version != input.Version {
			return application.ErrVersionConflict
		}
		row.Status = string(input.Status)
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = optional(input.OperatorID)
		result := tx.Model(&templateModel{}).Where("id = ? AND tenant_id = ? AND version = ?", row.ID, input.TenantID, input.Version).Select("status", "version", "updated_at", "updated_by").Updates(&row)
		if result.Error != nil {
			return fmt.Errorf("change notification template status: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		return nil
	})
	if err != nil {
		return domain.Template{}, err
	}
	return templateToDomain(row), nil
}

func (repository *Repository) ListTemplates(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.Template], error) {
	db := repository.database.WithContext(ctx).Model(&templateModel{}).Where("tenant_id = ?", tenantID)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.Template]{}, fmt.Errorf("count notification templates: %w", err)
	}
	var rows []templateModel
	if err := db.Order("updated_at DESC, id DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Template]{}, fmt.Errorf("list notification templates: %w", err)
	}
	items := make([]domain.Template, 0, len(rows))
	for _, row := range rows {
		items = append(items, templateToDomain(row))
	}
	return application.PageResult[domain.Template]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}

func (repository *Repository) GetActiveTemplateByCode(ctx context.Context, tenantID, code string) (domain.Template, domain.TemplateVersion, error) {
	var template templateModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", tenantID, code, domain.TemplateStatusActive).Take(&template).Error; err != nil {
		return domain.Template{}, domain.TemplateVersion{}, mapError(err)
	}
	var version templateVersionModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND template_id = ? AND version = ? AND status = ?", tenantID, template.ID, template.CurrentVersion, domain.TemplateVersionPublished).Take(&version).Error; err != nil {
		return domain.Template{}, domain.TemplateVersion{}, mapError(err)
	}
	mapped, err := templateVersionToDomain(version)
	if err != nil {
		return domain.Template{}, domain.TemplateVersion{}, err
	}
	return templateToDomain(template), mapped, nil
}

func (repository *Repository) CreateMessage(ctx context.Context, message domain.Message, deliveries []domain.Delivery) (application.MessageCreation, error) {
	// 租户内幂等键命中时返回原消息及投递，不创建第二套收件箱项；首次消息和全部收件人
	// 在一个事务中写入，不能留下只有消息而没有投递的半成品。
	var output application.MessageCreation
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := messageToModel(message)
		if err := tx.Create(&row).Error; err != nil {
			if !isDuplicate(err) {
				return mapError(err)
			}
			return loadMessageCreation(tx, message.TenantID, message.IdempotencyKey, &output)
		}
		if len(deliveries) == 0 {
			return application.ErrNoRecipients
		}
		rows := make([]deliveryModel, 0, len(deliveries))
		for _, delivery := range deliveries {
			rows = append(rows, deliveryToModel(delivery))
		}
		if err := tx.Create(&rows).Error; err != nil {
			return mapError(err)
		}
		output = application.MessageCreation{Message: message, Deliveries: deliveries}
		return nil
	})
	return output, err
}

func (repository *Repository) CompleteDelivery(ctx context.Context, tenantID, deliveryID string, now time.Time) (domain.Delivery, error) {
	// PENDING 与 PROCESSING 都可完成，以兼容首次同步投递和租约重试；其他终态不能被覆盖。
	delivered := now
	result := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("id = ? AND tenant_id = ? AND status IN ?", deliveryID, tenantID, []string{string(domain.DeliveryStatusPending), string(domain.DeliveryStatusProcessing)}).Updates(map[string]any{"status": string(domain.DeliveryStatusDelivered), "delivered_at": delivered, "next_retry_at": nil, "locked_until": nil, "last_error": "", "updated_at": now})
	if result.Error != nil {
		return domain.Delivery{}, fmt.Errorf("complete notification delivery: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.Delivery{}, application.ErrNotFound
	}
	var row deliveryModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND tenant_id = ?", deliveryID, tenantID).Take(&row).Error; err != nil {
		return domain.Delivery{}, mapError(err)
	}
	return deliveryToDomain(row), nil
}
func (repository *Repository) FailDelivery(ctx context.Context, tenantID, deliveryID, safeReason string, nextRetryAt, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("id = ? AND tenant_id = ? AND status IN ?", deliveryID, tenantID, []string{string(domain.DeliveryStatusPending), string(domain.DeliveryStatusProcessing)}).Updates(map[string]any{"status": string(domain.DeliveryStatusFailed), "attempt_count": gorm.Expr("attempt_count + 1"), "last_error": safeReason, "next_retry_at": nextRetryAt, "locked_until": nil, "updated_at": now})
	if result.Error != nil {
		return fmt.Errorf("fail notification delivery: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrNotFound
	}
	return nil
}
func (repository *Repository) ClaimFailedDeliveries(ctx context.Context, tenantID string, limit int, leaseUntil, now time.Time) ([]domain.Delivery, error) {
	// SKIP LOCKED 允许多个重试实例各取不同记录；next_retry_at 在 PROCESSING 阶段充当租约到期时间，
	// 进程崩溃后记录可被重新纳入领取条件。
	var rows []deliveryModel
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("tenant_id = ? AND status = ? AND next_retry_at <= ? AND (locked_until IS NULL OR locked_until < ?)", tenantID, domain.DeliveryStatusFailed, now, now).Order("next_retry_at ASC,id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return fmt.Errorf("claim failed notification deliveries: %w", err)
		}
		for i := range rows {
			result := tx.Model(&deliveryModel{}).Where("id = ? AND tenant_id = ? AND status = ?", rows[i].ID, tenantID, domain.DeliveryStatusFailed).Updates(map[string]any{"status": string(domain.DeliveryStatusProcessing), "locked_until": leaseUntil, "updated_at": now})
			if result.Error != nil {
				return fmt.Errorf("lock notification delivery: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return application.ErrConflict
			}
			rows[i].Status = string(domain.DeliveryStatusProcessing)
			rows[i].LockedUntil = &leaseUntil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	items := make([]domain.Delivery, 0, len(rows))
	for _, row := range rows {
		items = append(items, deliveryToDomain(row))
	}
	return items, nil
}
func (repository *Repository) ListDeliveries(ctx context.Context, tenantID string, status domain.DeliveryStatus, page application.PageRequest) (application.PageResult[domain.Delivery], error) {
	db := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("tenant_id = ?", tenantID)
	if status != "" {
		db = db.Where("status = ?", status)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.Delivery]{}, fmt.Errorf("count notification deliveries: %w", err)
	}
	var rows []deliveryModel
	if err := db.Order("updated_at DESC, id DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Delivery]{}, fmt.Errorf("list notification deliveries: %w", err)
	}
	items := make([]domain.Delivery, 0, len(rows))
	for _, row := range rows {
		items = append(items, deliveryToDomain(row))
	}
	return application.PageResult[domain.Delivery]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}

func (repository *Repository) ListInbox(ctx context.Context, tenantID, userID string, page application.PageRequest) (application.PageResult[domain.InboxItem], error) {
	base := repository.database.WithContext(ctx).Table("notification_delivery AS d").Joins("JOIN notification_message AS m ON m.id = d.message_id AND m.tenant_id = d.tenant_id").Where("d.tenant_id = ? AND d.recipient_user_id = ? AND d.status = ?", tenantID, userID, domain.DeliveryStatusDelivered)
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return application.PageResult[domain.InboxItem]{}, fmt.Errorf("count notification inbox: %w", err)
	}
	var rows []inboxRow
	if err := base.Select("d.id AS delivery_id,d.message_id,m.category,m.title,m.content,m.target_url,m.reference_type,m.reference_id,d.delivered_at,d.read_at").Order("d.delivered_at DESC,d.id DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Scan(&rows).Error; err != nil {
		return application.PageResult[domain.InboxItem]{}, fmt.Errorf("list notification inbox: %w", err)
	}
	items := make([]domain.InboxItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, inboxToDomain(row))
	}
	return application.PageResult[domain.InboxItem]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}
func (repository *Repository) GetInboxItem(ctx context.Context, tenantID, userID, deliveryID string) (domain.InboxItem, error) {
	return repository.inboxItem(ctx, tenantID, userID, deliveryID)
}

func (repository *Repository) CountUnread(ctx context.Context, tenantID, userID string) (int64, error) {
	var total int64
	if err := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("tenant_id = ? AND recipient_user_id = ? AND status = ? AND read_at IS NULL", tenantID, userID, domain.DeliveryStatusDelivered).Count(&total).Error; err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return total, nil
}
func (repository *Repository) MarkRead(ctx context.Context, tenantID, userID, deliveryID string, now time.Time) (domain.InboxItem, error) {
	result := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("id = ? AND tenant_id = ? AND recipient_user_id = ? AND status = ? AND read_at IS NULL", deliveryID, tenantID, userID, domain.DeliveryStatusDelivered).Updates(map[string]any{"read_at": now, "updated_at": now})
	if result.Error != nil {
		return domain.InboxItem{}, fmt.Errorf("mark notification read: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("id = ? AND tenant_id = ? AND recipient_user_id = ? AND status = ?", deliveryID, tenantID, userID, domain.DeliveryStatusDelivered).Count(&count).Error; err != nil {
			return domain.InboxItem{}, fmt.Errorf("check notification inbox ownership: %w", err)
		}
		if count == 0 {
			return domain.InboxItem{}, application.ErrNotFound
		}
	}
	return repository.inboxItem(ctx, tenantID, userID, deliveryID)
}
func (repository *Repository) MarkAllRead(ctx context.Context, tenantID, userID string, now time.Time) (int64, error) {
	result := repository.database.WithContext(ctx).Model(&deliveryModel{}).Where("tenant_id = ? AND recipient_user_id = ? AND status = ? AND read_at IS NULL", tenantID, userID, domain.DeliveryStatusDelivered).Updates(map[string]any{"read_at": now, "updated_at": now})
	if result.Error != nil {
		return 0, fmt.Errorf("mark all notifications read: %w", result.Error)
	}
	return result.RowsAffected, nil
}
func (repository *Repository) inboxItem(ctx context.Context, tenantID, userID, deliveryID string) (domain.InboxItem, error) {
	var row inboxRow
	err := repository.database.WithContext(ctx).Table("notification_delivery AS d").Joins("JOIN notification_message AS m ON m.id = d.message_id AND m.tenant_id = d.tenant_id").Where("d.id = ? AND d.tenant_id = ? AND d.recipient_user_id = ? AND d.status = ?", deliveryID, tenantID, userID, domain.DeliveryStatusDelivered).Select("d.id AS delivery_id,d.message_id,m.category,m.title,m.content,m.target_url,m.reference_type,m.reference_id,d.delivered_at,d.read_at").Scan(&row).Error
	if err != nil {
		return domain.InboxItem{}, fmt.Errorf("get notification inbox item: %w", err)
	}
	if row.DeliveryID == "" {
		return domain.InboxItem{}, application.ErrNotFound
	}
	return inboxToDomain(row), nil
}

func loadMessageCreation(tx *gorm.DB, tenantID, key string, output *application.MessageCreation) error {
	var message messageModel
	if err := tx.Where("tenant_id = ? AND idempotency_key = ?", tenantID, key).Take(&message).Error; err != nil {
		return mapError(err)
	}
	var rows []deliveryModel
	if err := tx.Where("tenant_id = ? AND message_id = ?", tenantID, message.ID).Order("id ASC").Find(&rows).Error; err != nil {
		return fmt.Errorf("load idempotent notification deliveries: %w", err)
	}
	deliveries := make([]domain.Delivery, 0, len(rows))
	for _, row := range rows {
		deliveries = append(deliveries, deliveryToDomain(row))
	}
	*output = application.MessageCreation{Message: messageToDomain(message), Deliveries: deliveries, Replayed: true}
	return nil
}
func marshalVariables(values []domain.VariableDefinition) ([]byte, error) {
	data, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal notification template variables: %w", err)
	}
	return data, nil
}
func templateVersionToDomain(row templateVersionModel) (domain.TemplateVersion, error) {
	var variables []domain.VariableDefinition
	if err := json.Unmarshal(row.VariablesJSON, &variables); err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("decode notification template variables: %w", err)
	}
	return domain.TemplateVersion{ID: row.ID, TemplateID: row.TemplateID, TenantID: row.TenantID, Version: row.Version, Status: domain.TemplateVersionStatus(row.Status), TitleTemplate: row.TitleTemplate, BodyTemplate: row.BodyTemplate, Variables: variables, PublishedAt: row.PublishedAt, CreatedAt: row.CreatedAt, CreatedBy: value(row.CreatedBy)}, nil
}
func templateToDomain(row templateModel) domain.Template {
	return domain.Template{ID: row.ID, TenantID: row.TenantID, Code: row.Code, Name: row.Name, Status: domain.TemplateStatus(row.Status), CurrentVersion: row.CurrentVersion, Version: row.Version, CreatedAt: row.CreatedAt, CreatedBy: value(row.CreatedBy), UpdatedAt: row.UpdatedAt, UpdatedBy: value(row.UpdatedBy)}
}
func messageToModel(value domain.Message) messageModel {
	return messageModel{ID: value.ID, TenantID: value.TenantID, TemplateID: value.TemplateID, TemplateVersionID: value.TemplateVersionID, Category: value.Category, Title: value.Title, Content: value.Content, TargetURL: value.TargetURL, ReferenceType: value.ReferenceType, ReferenceID: value.ReferenceID, IdempotencyKey: value.IdempotencyKey, CreatedAt: value.CreatedAt, CreatedBy: optional(value.CreatedBy)}
}
func messageToDomain(row messageModel) domain.Message {
	return domain.Message{ID: row.ID, TenantID: row.TenantID, TemplateID: row.TemplateID, TemplateVersionID: row.TemplateVersionID, Category: row.Category, Title: row.Title, Content: row.Content, TargetURL: row.TargetURL, ReferenceType: row.ReferenceType, ReferenceID: row.ReferenceID, IdempotencyKey: row.IdempotencyKey, CreatedAt: row.CreatedAt, CreatedBy: value(row.CreatedBy)}
}
func deliveryToModel(value domain.Delivery) deliveryModel {
	return deliveryModel{ID: value.ID, TenantID: value.TenantID, MessageID: value.MessageID, RecipientUserID: value.RecipientUserID, Status: string(value.Status), AttemptCount: value.AttemptCount, LastError: value.LastError, NextRetryAt: value.NextRetryAt, LockedUntil: value.LockedUntil, DeliveredAt: value.DeliveredAt, ReadAt: value.ReadAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}
func deliveryToDomain(row deliveryModel) domain.Delivery {
	return domain.Delivery{ID: row.ID, TenantID: row.TenantID, MessageID: row.MessageID, RecipientUserID: row.RecipientUserID, Status: domain.DeliveryStatus(row.Status), AttemptCount: row.AttemptCount, LastError: row.LastError, NextRetryAt: row.NextRetryAt, LockedUntil: row.LockedUntil, DeliveredAt: row.DeliveredAt, ReadAt: row.ReadAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
func inboxToDomain(row inboxRow) domain.InboxItem {
	return domain.InboxItem{DeliveryID: row.DeliveryID, MessageID: row.MessageID, Category: row.Category, Title: row.Title, Content: row.Content, TargetURL: row.TargetURL, ReferenceType: row.ReferenceType, ReferenceID: row.ReferenceID, DeliveredAt: row.DeliveredAt, ReadAt: row.ReadAt}
}
func optional(raw string) *string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	return &value
}
func value(raw *string) string {
	if raw == nil {
		return ""
	}
	return *raw
}
func isDuplicate(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(err.Error(), "1062")
}
func mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	if isDuplicate(err) {
		return application.ErrConflict
	}
	return err
}
