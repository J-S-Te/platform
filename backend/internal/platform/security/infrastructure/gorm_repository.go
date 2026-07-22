package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	loginAttemptFailure = "FAILURE"
	riskEventAccount    = "ACCOUNT"
	riskLevelHigh       = "HIGH"
	riskLevelLow        = "LOW"
)

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
func (repository *GORMRepository) RecordFailedLogin(ctx context.Context, input securityapplication.LoginFailureInput, policy securitydomain.LoginPolicy, riskEventID string, now time.Time) (securityapplication.LoginFailureResult, error) {
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
			RiskScore:     20,
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
		metadata, err := json.Marshal(map[string]any{"failure_count": failures, "locked_until": lockedUntil.Format(time.RFC3339Nano)})
		if err != nil {
			return fmt.Errorf("marshal lock risk metadata: %w", err)
		}
		accountID := input.AccountID
		event := riskEventModel{
			ID:            riskEventID,
			TenantID:      input.TenantID,
			AccountID:     &accountID,
			EventType:     "ACCOUNT_LOCKED",
			SubjectType:   riskEventAccount,
			SubjectID:     input.AccountID,
			RiskLevel:     riskLevelHigh,
			RiskScore:     70,
			SourceIP:      ipBytes(input.IPAddress),
			DetectionRule: "LOGIN_FAILURE_THRESHOLD",
			Status:        securitydomain.RiskEventStatusOpen,
			OccurredAt:    now,
			Metadata:      metadata,
			Version:       1,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := transaction.Create(&event).Error; err != nil {
			return fmt.Errorf("create account lock risk event: %w", err)
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
func (repository *GORMRepository) UnlockAccount(ctx context.Context, input securityapplication.UnlockInput, eventID string, now time.Time) (securitydomain.LockedAccount, error) {
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
		comment := "管理员已解除账号锁定"
		if err := transaction.Model(&riskEventModel{}).
			Where("tenant_id = ? AND account_id = ? AND event_type = ? AND status = ?", input.TenantID, input.AccountID, "ACCOUNT_LOCKED", securitydomain.RiskEventStatusOpen).
			Updates(map[string]any{"status": securitydomain.RiskEventStatusResolved, "resolved_at": now, "resolved_by": input.OperatorID, "resolution_comment": comment, "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			return fmt.Errorf("resolve lock risk events: %w", err)
		}
		metadata, err := json.Marshal(map[string]any{"unlocked_account_id": input.AccountID})
		if err != nil {
			return fmt.Errorf("marshal unlock risk metadata: %w", err)
		}
		accountID := input.AccountID
		operatorID := input.OperatorID
		event := riskEventModel{ID: eventID, TenantID: input.TenantID, AccountID: &accountID, EventType: "ACCOUNT_UNLOCKED", SubjectType: riskEventAccount, SubjectID: input.AccountID, RiskLevel: riskLevelLow, RiskScore: 0, DetectionRule: "ADMINISTRATIVE_UNLOCK", Status: securitydomain.RiskEventStatusResolved, OccurredAt: now, ResolvedAt: &now, ResolvedBy: &operatorID, ResolutionComment: &comment, Metadata: metadata, Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := transaction.Create(&event).Error; err != nil {
			return fmt.Errorf("create account unlock risk event: %w", err)
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

// ListRiskEvents retrieves sanitized, tenant-scoped operator evidence.
func (repository *GORMRepository) ListRiskEvents(ctx context.Context, tenantID string, query securityapplication.PageRequest) (securityapplication.PageResult[securitydomain.RiskEvent], error) {
	base := repository.database.WithContext(ctx).Table("sec_risk_event AS event").Where("event.tenant_id = ?", tenantID)
	if query.Status != "" {
		base = base.Where("event.status = ?", query.Status)
	}
	if query.Keyword != "" {
		like := "%" + query.Keyword + "%"
		base = base.Where("event.event_type LIKE ? OR event.subject_id LIKE ?", like, like)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return securityapplication.PageResult[securitydomain.RiskEvent]{}, fmt.Errorf("count risk events: %w", err)
	}
	var rows []riskEventProjection
	if err := base.Select(`event.*, account.username AS account_name`).Joins("LEFT JOIN iam_account AS account ON account.id = event.account_id AND account.tenant_id = event.tenant_id").
		Order("event.occurred_at DESC, event.id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return securityapplication.PageResult[securitydomain.RiskEvent]{}, fmt.Errorf("list risk events: %w", err)
	}
	items := make([]securitydomain.RiskEvent, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.toDomain())
	}
	return securityapplication.PageResult[securitydomain.RiskEvent]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// ResolveRiskEvent persists an explicit operator resolution with optimistic locking.
func (repository *GORMRepository) ResolveRiskEvent(ctx context.Context, input securityapplication.RiskEventResolveInput, now time.Time) (securitydomain.RiskEvent, error) {
	var resolved securitydomain.RiskEvent
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row riskEventModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ?", input.RiskEventID, input.TenantID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return securityapplication.ErrNotFound
			}
			return fmt.Errorf("lock risk event for resolution: %w", err)
		}
		if row.Status != securitydomain.RiskEventStatusOpen || row.Version != input.Version {
			return securityapplication.ErrConflict
		}
		operatorID := input.OperatorID
		comment := strings.TrimSpace(input.ResolutionComment)
		row.Status = securitydomain.RiskEventStatusResolved
		row.ResolvedAt = &now
		row.ResolvedBy = &operatorID
		row.ResolutionComment = &comment
		row.Version++
		row.UpdatedAt = now
		if err := transaction.Save(&row).Error; err != nil {
			return fmt.Errorf("resolve risk event: %w", err)
		}
		resolved = riskEventToDomain(row, nil)
		return nil
	})
	if err != nil {
		return securitydomain.RiskEvent{}, err
	}
	return resolved, nil
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

type riskEventProjection struct {
	riskEventModel
	AccountName *string `gorm:"column:account_name"`
}

func (projection riskEventProjection) toDomain() securitydomain.RiskEvent {
	return riskEventToDomain(projection.riskEventModel, projection.AccountName)
}

func loginPolicyToDomain(row loginPolicyModel) securitydomain.LoginPolicy {
	return securitydomain.LoginPolicy{TenantID: row.TenantID, MaxFailedAttempts: row.MaxFailedAttempts, LockoutDurationSeconds: row.LockoutDurationSeconds, FailureResetWindowSeconds: row.FailureResetWindowSeconds, Version: row.Version, UpdatedAt: row.UpdatedAt}
}

func riskEventToDomain(row riskEventModel, accountName *string) securitydomain.RiskEvent {
	metadata := map[string]any{}
	if len(row.Metadata) > 0 {
		_ = json.Unmarshal(row.Metadata, &metadata)
	}
	return securitydomain.RiskEvent{RiskEventID: row.ID, TenantID: row.TenantID, AccountID: row.AccountID, AccountName: accountName, EventType: row.EventType, SubjectType: row.SubjectType, SubjectID: row.SubjectID, RiskLevel: row.RiskLevel, RiskScore: row.RiskScore, SourceIP: ipString(row.SourceIP), DetectionRule: row.DetectionRule, Status: row.Status, OccurredAt: row.OccurredAt, ResolvedAt: row.ResolvedAt, ResolvedBy: row.ResolvedBy, ResolutionComment: row.ResolutionComment, Metadata: metadata, Version: row.Version}
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

func ipString(value []byte) *string {
	if len(value) == 0 {
		return nil
	}
	address := net.IP(value).String()
	return &address
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
