package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const loginAttemptFailure = "FAILURE"

// GORMRepository implements security application persistence against MySQL through GORM.
type GORMRepository struct {
	database *gorm.DB
}

// NewGORMRepository validates the database dependency and returns a security repository.
func NewGORMRepository(database *gorm.DB) (*GORMRepository, error) {
	if database == nil {
		return nil, errors.New("security GORM database must not be nil")
	}
	return &GORMRepository{database: database}, nil
}

// GetLoginPolicy reads a tenant policy without fabricating a default at the persistence layer.
func (repository *GORMRepository) GetLoginPolicy(ctx context.Context, tenantID string) (securitydomain.LoginPolicy, error) {
	var row loginPolicyModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return securitydomain.LoginPolicy{}, securityapplication.ErrNotFound
		}
		return securitydomain.LoginPolicy{}, fmt.Errorf("get login policy: %w", err)
	}
	return loginPolicyToDomain(row), nil
}

// UpdateLoginPolicy applies an optimistic-lock update, creating the first persisted policy from
// the documented default version when a tenant has not customized it before.
func (repository *GORMRepository) UpdateLoginPolicy(ctx context.Context, input securityapplication.LoginPolicyUpdateInput, now time.Time) (securitydomain.LoginPolicy, error) {
	var result securitydomain.LoginPolicy
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row loginPolicyModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", input.TenantID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.Version != 1 {
				return securityapplication.ErrConflict
			}
			createdBy := input.OperatorID
			row = loginPolicyModel{
				TenantID:                  input.TenantID,
				MaxFailedAttempts:         input.MaxFailedAttempts,
				LockoutDurationSeconds:    input.LockoutDurationSeconds,
				FailureResetWindowSeconds: input.FailureResetWindowSeconds,
				IdleTimeoutSeconds:        input.IdleTimeoutSeconds,
				Version:                   2,
				CreatedAt:                 now,
				CreatedBy:                 &createdBy,
				UpdatedAt:                 now,
				UpdatedBy:                 &createdBy,
			}
			if err := transaction.Create(&row).Error; err != nil {
				return fmt.Errorf("create login policy: %w", err)
			}
			result = loginPolicyToDomain(row)
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock login policy: %w", err)
		}
		if row.Version != input.Version {
			return securityapplication.ErrConflict
		}
		updatedBy := input.OperatorID
		row.MaxFailedAttempts = input.MaxFailedAttempts
		row.LockoutDurationSeconds = input.LockoutDurationSeconds
		row.FailureResetWindowSeconds = input.FailureResetWindowSeconds
		row.IdleTimeoutSeconds = input.IdleTimeoutSeconds
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &updatedBy
		if err := transaction.Save(&row).Error; err != nil {
			return fmt.Errorf("update login policy: %w", err)
		}
		result = loginPolicyToDomain(row)
		return nil
	})
	if err != nil {
		return securitydomain.LoginPolicy{}, err
	}
	return result, nil
}

// RecordFailedLogin stores an invalid-password attempt and locks the account atomically when the
// tenant threshold is met. The password itself is never persisted.
func (repository *GORMRepository) RecordFailedLogin(ctx context.Context, input securityapplication.LoginFailureInput, policy securitydomain.LoginPolicy, now time.Time) (securityapplication.LoginFailureResult, error) {
	var result securityapplication.LoginFailureResult
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var account struct {
			LockedUntil *time.Time `gorm:"column:locked_until"`
		}
		if err := transaction.Table("iam_account").Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("locked_until").Where("id = ? AND tenant_id = ?", input.AccountID, input.TenantID).Take(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return securityapplication.ErrNotFound
			}
			return fmt.Errorf("lock login account: %w", err)
		}
		if account.LockedUntil != nil && account.LockedUntil.After(now) {
			result.LockedUntil = account.LockedUntil
			return nil
		}

		resetBefore := now.Add(-time.Duration(policy.FailureResetWindowSeconds) * time.Second)
		if err := transaction.Model(&loginAttemptModel{}).
			Where("tenant_id = ? AND account_id = ? AND result = ? AND cleared_at IS NULL AND occurred_at < ?", input.TenantID, input.AccountID, loginAttemptFailure, resetBefore).
			Update("cleared_at", now).Error; err != nil {
			return fmt.Errorf("clear expired login failures: %w", err)
		}

		attempt := loginAttemptModel{
			OccurredAt:    now,
			TenantID:      input.TenantID,
			AccountID:     input.AccountID,
			Username:      truncate(input.AccountName, 128),
			IPAddress:     ipBytes(input.IPAddress),
			UserAgent:     truncate(input.UserAgent, 1000),
			Result:        loginAttemptFailure,
			FailureReason: "INVALID_PASSWORD",
		}
		if err := transaction.Create(&attempt).Error; err != nil {
			return fmt.Errorf("create login failure: %w", err)
		}

		var failures int64
		if err := transaction.Model(&loginAttemptModel{}).
			Where("tenant_id = ? AND account_id = ? AND result = ? AND cleared_at IS NULL", input.TenantID, input.AccountID, loginAttemptFailure).
			Count(&failures).Error; err != nil {
			return fmt.Errorf("count active login failures: %w", err)
		}
		if failures < int64(policy.MaxFailedAttempts) {
			return nil
		}

		lockedUntil := now.Add(time.Duration(policy.LockoutDurationSeconds) * time.Second)
		if err := transaction.Table("iam_account").Where("id = ? AND tenant_id = ?", input.AccountID, input.TenantID).
			Updates(map[string]any{"locked_until": lockedUntil, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("lock account after login failures: %w", err)
		}
		// Existing browser sessions must not remain usable while an account is locked.
		// Keep this in the same transaction as the lock so a failed session revocation rolls back
		// the lock instead of creating an ambiguous partially applied security state.
		if err := transaction.Table("iam_session").
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", input.TenantID, input.AccountID, "ACTIVE").
			Updates(map[string]any{"status": "REVOKED", "revoked_at": now, "revoke_reason": "ACCOUNT_LOCKED"}).Error; err != nil {
			return fmt.Errorf("revoke sessions after account lock: %w", err)
		}
		result.LockedUntil = &lockedUntil
		return nil
	})
	if err != nil {
		return securityapplication.LoginFailureResult{}, err
	}
	return result, nil
}

// ListLockedAccounts returns accounts whose lock window has not expired.
func (repository *GORMRepository) ListLockedAccounts(ctx context.Context, tenantID string, query securityapplication.PageRequest, now time.Time) (securityapplication.PageResult[securitydomain.LockedAccount], error) {
	base := repository.database.WithContext(ctx).Table("iam_account AS account").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
		Where("account.tenant_id = ? AND account.locked_until > ?", tenantID, now)
	if query.Keyword != "" {
		like := "%" + query.Keyword + "%"
		base = base.Where("account.username LIKE ? OR user.display_name LIKE ?", like, like)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return securityapplication.PageResult[securitydomain.LockedAccount]{}, fmt.Errorf("count locked accounts: %w", err)
	}
	var rows []lockedAccountProjection
	selectSQL := `account.id AS account_id, account.username AS account_name, user.id AS user_id, user.display_name AS user_name,
		account.locked_until,
		(SELECT MAX(attempt.occurred_at) FROM sec_login_attempt AS attempt WHERE attempt.tenant_id = account.tenant_id AND attempt.account_id = account.id AND attempt.result = 'FAILURE' AND attempt.cleared_at IS NULL) AS last_failed_at,
		(SELECT COUNT(*) FROM sec_login_attempt AS attempt WHERE attempt.tenant_id = account.tenant_id AND attempt.account_id = account.id AND attempt.result = 'FAILURE' AND attempt.cleared_at IS NULL) AS failure_count`
	if err := base.Select(selectSQL).Order("account.locked_until ASC, account.id ASC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return securityapplication.PageResult[securitydomain.LockedAccount]{}, fmt.Errorf("list locked accounts: %w", err)
	}
	items := make([]securitydomain.LockedAccount, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.toDomain())
	}
	return securityapplication.PageResult[securitydomain.LockedAccount]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// UnlockAccount clears the lock and active failures, resolves unresolved lock events, and creates
// a non-sensitive administrative disposition event in the same database transaction.
func (repository *GORMRepository) UnlockAccount(ctx context.Context, input securityapplication.UnlockInput, now time.Time) (securitydomain.LockedAccount, error) {
	var unlocked securitydomain.LockedAccount
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var account lockedAccountProjection
		if err := transaction.Table("iam_account AS account").Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("account.id AS account_id, account.username AS account_name, user.id AS user_id, user.display_name AS user_name, account.locked_until").
			Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
			Where("account.id = ? AND account.tenant_id = ?", input.AccountID, input.TenantID).Take(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return securityapplication.ErrNotFound
			}
			return fmt.Errorf("lock account for administrative unlock: %w", err)
		}
		if account.LockedUntil == nil || !account.LockedUntil.After(now) {
			return securityapplication.ErrConflict
		}
		if err := transaction.Table("iam_account").Where("id = ? AND tenant_id = ?", input.AccountID, input.TenantID).
			Updates(map[string]any{"locked_until": nil, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("clear account lock: %w", err)
		}
		if err := transaction.Table("iam_password_credential").Where("account_id = ?", input.AccountID).
			Updates(map[string]any{"failed_attempts": 0, "last_failed_at": nil, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("reset password credential failures: %w", err)
		}
		if err := transaction.Model(&loginAttemptModel{}).
			Where("tenant_id = ? AND account_id = ? AND result = ? AND cleared_at IS NULL", input.TenantID, input.AccountID, loginAttemptFailure).
			Update("cleared_at", now).Error; err != nil {
			return fmt.Errorf("clear login failures after manual unlock: %w", err)
		}
		unlocked = account.toDomain()
		unlocked.LockedUntil = nil
		unlocked.FailureCount = 0
		unlocked.LastFailedAt = nil
		return nil
	})
	if err != nil {
		return securitydomain.LockedAccount{}, err
	}
	return unlocked, nil
}

type lockedAccountProjection struct {
	AccountID    string     `gorm:"column:account_id"`
	AccountName  string     `gorm:"column:account_name"`
	UserID       string     `gorm:"column:user_id"`
	UserName     string     `gorm:"column:user_name"`
	LockedUntil  *time.Time `gorm:"column:locked_until"`
	LastFailedAt *time.Time `gorm:"column:last_failed_at"`
	FailureCount uint       `gorm:"column:failure_count"`
}

func (projection lockedAccountProjection) toDomain() securitydomain.LockedAccount {
	return securitydomain.LockedAccount{AccountID: projection.AccountID, AccountName: projection.AccountName, UserID: projection.UserID, UserName: projection.UserName, LockedUntil: projection.LockedUntil, LastFailedAt: projection.LastFailedAt, FailureCount: projection.FailureCount}
}

func loginPolicyToDomain(row loginPolicyModel) securitydomain.LoginPolicy {
	return securitydomain.LoginPolicy{TenantID: row.TenantID, MaxFailedAttempts: row.MaxFailedAttempts, LockoutDurationSeconds: row.LockoutDurationSeconds, FailureResetWindowSeconds: row.FailureResetWindowSeconds, IdleTimeoutSeconds: row.IdleTimeoutSeconds, Version: row.Version, UpdatedAt: row.UpdatedAt}
}

func ipBytes(address net.IP) []byte {
	if address == nil {
		return nil
	}
	if ipv4 := address.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	if ipv6 := address.To16(); ipv6 != nil {
		return append([]byte(nil), ipv6...)
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
