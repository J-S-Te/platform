package infrastructure

import (
	"context"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"gorm.io/gorm"
)

// ExpiryGORMRepository 以 GORM 扫描与标记身份有效期到期记录，供过期 worker 轮询。
type ExpiryGORMRepository struct{ database *gorm.DB }

// NewExpiryGORMRepository 构造过期仓储。
func NewExpiryGORMRepository(database *gorm.DB) *ExpiryGORMRepository {
	return &ExpiryGORMRepository{database: database}
}

// ListDueExpirations 返回已到 valid_until 且尚未处理的有效期候选，用户优先于账号，均按
// 到期时间与 ID 稳定排序。单次返回上限 200，避免一轮扫描长时间占用数据库。
func (repository *ExpiryGORMRepository) ListDueExpirations(ctx context.Context, now time.Time, limit int) ([]application.ExpiryCandidate, error) {
	if repository == nil || repository.database == nil || limit < 1 {
		return nil, application.ErrValidation
	}
	if limit > 200 {
		limit = 200
	}
	now = now.UTC()
	var users []userModel
	if err := repository.database.WithContext(ctx).Where("status = ? AND deleted_at IS NULL AND valid_until IS NOT NULL AND valid_until <= ? AND expiry_processed_at IS NULL", "ACTIVE", now).Order("valid_until, id").Limit(limit).Find(&users).Error; err != nil {
		return nil, fmt.Errorf("list expired users: %w", err)
	}
	result := make([]application.ExpiryCandidate, 0, limit)
	for _, user := range users {
		var accountIDs []string
		if err := repository.database.WithContext(ctx).Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", user.TenantID, user.ID).Pluck("id", &accountIDs).Error; err != nil {
			return nil, fmt.Errorf("list expired user accounts: %w", err)
		}
		result = append(result, application.ExpiryCandidate{Kind: application.ExpiryKindUser, TenantID: user.TenantID, ResourceID: user.ID, UserID: user.ID, ValidUntil: user.ValidUntil.UTC(), AccountIDs: accountIDs})
	}
	remaining := limit - len(result)
	if remaining <= 0 {
		return result, nil
	}
	var accounts []accountModel
	if err := repository.database.WithContext(ctx).Where("status = ? AND valid_until IS NOT NULL AND valid_until <= ? AND expiry_processed_at IS NULL", "ACTIVE", now).Order("valid_until, id").Limit(remaining).Find(&accounts).Error; err != nil {
		return nil, fmt.Errorf("list expired accounts: %w", err)
	}
	for _, account := range accounts {
		userID := ""
		if account.UserID != nil {
			userID = *account.UserID
		}
		result = append(result, application.ExpiryCandidate{Kind: application.ExpiryKindAccount, TenantID: account.TenantID, ResourceID: account.ID, UserID: userID, ValidUntil: account.ValidUntil.UTC(), AccountIDs: []string{account.ID}})
	}
	return result, nil
}

// MarkExpirationProcessed 以条件更新把候选标记为已处理：条件同时绑定 valid_until 与
// expiry_processed_at IS NULL，因此只有尚未被并发 worker 处理的行会更新；不校验影响行数，
// 重复调用天然幂等。
func (repository *ExpiryGORMRepository) MarkExpirationProcessed(ctx context.Context, candidate application.ExpiryCandidate, processedAt time.Time) error {
	table := "iam_account"
	if candidate.Kind == application.ExpiryKindUser {
		table = "iam_user"
	}
	result := repository.database.WithContext(ctx).Table(table).
		Where("tenant_id = ? AND id = ? AND valid_until = ? AND expiry_processed_at IS NULL", candidate.TenantID, candidate.ResourceID, candidate.ValidUntil.UTC()).
		Update("expiry_processed_at", processedAt.UTC())
	if result.Error != nil {
		return fmt.Errorf("mark identity expiration processed: %w", result.Error)
	}
	return nil
}
