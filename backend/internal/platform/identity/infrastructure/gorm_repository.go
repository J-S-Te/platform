// Package infrastructure provides GORM-backed identity repositories.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const consoleApplicationCode = "platform"

// GORMRepository persists identity credentials, organization data and browser-session state in the
// migration-owned platform schema. It does not use GORM schema migration APIs.
type GORMRepository struct {
	database *gorm.DB
}

// NewGORMRepository creates an identity repository using the application's shared GORM handle.
func NewGORMRepository(database *gorm.DB) (*GORMRepository, error) {
	if database == nil {
		return nil, errors.New("identity GORM database must not be nil")
	}
	return &GORMRepository{database: database}, nil
}

// FindLoginAccount returns a local account and password credential for a supplied username. It
// deliberately does not filter account status because the application layer distinguishes an active
// lock window from generic authentication failures without exposing other account state.
func (repository *GORMRepository) FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error) {
	var row loginAccountProjection
	result := repository.database.WithContext(ctx).
		Table("iam_account AS account").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code, tenant.status AS tenant_status,
			user.id AS user_id, user.display_name AS user_name, user.status AS user_status,
			account.id AS account_id, COALESCE(account.username, account.id) AS account_name, account.status AS account_status, account.locked_until,
			credential.password_hash, credential.hash_algorithm, credential.algorithm_params,
			credential.status AS credential_status, credential.expires_at AS credential_expiry`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = account.tenant_id").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
		Joins("JOIN iam_password_credential AS credential ON credential.account_id = account.id").
		Where("account.username = ? AND account.auth_source = ?", accountName, "LOCAL").
		Limit(1).
		Find(&row)
	if result.Error != nil {
		return domain.LoginAccount{}, fmt.Errorf("query local account credential: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.LoginAccount{}, application.ErrUnauthenticated
	}
	return toDomainLoginAccount(row), nil
}

// FindFederatedLoginAccount returns the exact local identity selected by a verified external
// identity binding. It deliberately does not load a password credential because external login
// eligibility is based on the current tenant, user, account and lock state.
func (repository *GORMRepository) FindFederatedLoginAccount(ctx context.Context, tenantID, userID, accountID string) (domain.LoginAccount, error) {
	var row loginAccountProjection
	result := repository.database.WithContext(ctx).
		Table("iam_account AS account").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code, tenant.status AS tenant_status,
			user.id AS user_id, user.display_name AS user_name, user.status AS user_status,
			account.id AS account_id, COALESCE(account.username, account.id) AS account_name, account.status AS account_status, account.locked_until`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = account.tenant_id").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
		Where("account.tenant_id = ? AND account.user_id = ? AND account.id = ?", tenantID, userID, accountID).
		Limit(1).
		Find(&row)
	if result.Error != nil {
		return domain.LoginAccount{}, fmt.Errorf("query federated login account: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.LoginAccount{}, application.ErrUnauthenticated
	}
	return toDomainLoginAccount(row), nil
}

// RecordSuccessfulPasswordVerification clears password-failure state immediately after the primary
// credential succeeds, including when MFA postpones browser-session creation.
func (repository *GORMRepository) RecordSuccessfulPasswordVerification(ctx context.Context, account domain.LoginAccount, now time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		verifiedAt := now.UTC()
		var credential passwordCredentialModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("account_id = ? AND status = ?", account.AccountID, domain.StatusActive).
			Where("expires_at IS NULL OR expires_at > ?", verifiedAt).
			Limit(1).
			Find(&credential)
		if result.Error != nil {
			return fmt.Errorf("lock successful password credential state: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrUnauthenticated
		}
		if err := transaction.Model(&passwordCredentialModel{}).
			Where("id = ?", credential.ID).
			Updates(map[string]any{"failed_attempts": 0, "last_failed_at": nil, "updated_at": verifiedAt}).Error; err != nil {
			return fmt.Errorf("reset successful password credential state: %w", err)
		}

		if err := transaction.Table("sec_login_attempt").
			Where("tenant_id = ? AND account_id = ? AND result = ? AND cleared_at IS NULL", account.TenantID, account.AccountID, "FAILURE").
			Update("cleared_at", verifiedAt).Error; err != nil {
			return fmt.Errorf("clear failed login attempts after successful password verification: %w", err)
		}
		return nil
	})
}

// CreateSession records a successful local or external account login and inserts iam_session. The
// status predicates prevent a concurrent disable or lock operation from creating a usable session.
func (repository *GORMRepository) CreateSession(ctx context.Context, account domain.LoginAccount, session domain.Session) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		now := session.CreatedAt.UTC()
		activeUser := transaction.Model(&userModel{}).
			Select("1").
			Where("id = iam_account.user_id AND tenant_id = iam_account.tenant_id AND status = ?", domain.StatusActive)
		activeTenant := transaction.Model(&tenantModel{}).
			Select("1").
			Where("id = iam_account.tenant_id AND status = ?", domain.StatusActive)

		var persistedAccount accountModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("id = ? AND tenant_id = ? AND user_id = ? AND status = ?", account.AccountID, account.TenantID, account.UserID, domain.StatusActive).
			Where("locked_until IS NULL OR locked_until <= ?", now).
			Where("EXISTS (?)", activeUser).
			Where("EXISTS (?)", activeTenant).
			Limit(1).
			Find(&persistedAccount)
		if result.Error != nil {
			return fmt.Errorf("lock account for successful login: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrUnauthenticated
		}
		if err := transaction.Model(&accountModel{}).
			Where("id = ?", persistedAccount.ID).
			Updates(map[string]any{"last_login_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("update account successful login: %w", err)
		}

		userAgent := session.UserAgent
		if err := transaction.Create(&sessionModel{
			ID:         session.ID,
			TenantID:   session.TenantID,
			AccountID:  session.AccountID,
			IPAddress:  nullableBytes(session.IPAddress),
			UserAgent:  nullableString(&userAgent),
			CreatedAt:  now,
			LastSeenAt: now,
			ExpiresAt:  session.ExpiresAt.UTC(),
			Status:     domain.StatusActive,
		}).Error; err != nil {
			return fmt.Errorf("insert browser session: %w", err)
		}
		return nil
	})
}

// FindPrincipalBySession verifies current session and account state, then loads the platform
// application's active role and permission summary.
func (repository *GORMRepository) FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time) (domain.Principal, error) {
	var row principalProjection
	result := repository.database.WithContext(ctx).
		Table("iam_session AS session").
		Select(`session.id AS session_id, tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code,
			user.id AS user_id, user.display_name AS user_name, account.id AS account_id, COALESCE(account.username, account.id) AS account_name`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = session.tenant_id AND tenant.status = ?", domain.StatusActive).
		Joins("JOIN iam_account AS account ON account.id = session.account_id AND account.tenant_id = session.tenant_id AND account.status = ?", domain.StatusActive).
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = session.tenant_id AND user.status = ?", domain.StatusActive).
		Where("session.id = ? AND session.status = ?", sessionID, domain.StatusActive).
		Where("session.revoked_at IS NULL AND session.expires_at > ?", now.UTC()).
		Limit(1).
		Find(&row)
	if result.Error != nil {
		return domain.Principal{}, fmt.Errorf("query authenticated session: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.Principal{}, application.ErrUnauthenticated
	}

	roles, err := repository.findRoles(ctx, row.TenantID, row.UserID, now)
	if err != nil {
		return domain.Principal{}, err
	}
	permissions, err := repository.findPermissionCodes(ctx, row.TenantID, row.UserID, now)
	if err != nil {
		return domain.Principal{}, err
	}
	return domain.Principal{
		SessionID:       row.SessionID,
		Tenant:          domain.ReferenceName{ID: row.TenantID, Name: row.TenantName, Code: row.TenantCode},
		User:            domain.ReferenceName{ID: row.UserID, Name: row.UserName},
		Account:         domain.ReferenceName{ID: row.AccountID, Name: row.AccountName},
		Roles:           roles,
		PermissionCodes: permissions,
	}, nil
}

// RefreshSession extends an existing active session after middleware has checked its JWT and
// current account state.
func (repository *GORMRepository) RefreshSession(ctx context.Context, sessionID string, refreshedAt, expiresAt time.Time) error {
	result := repository.database.WithContext(ctx).
		Model(&sessionModel{}).
		Where("id = ? AND status = ?", sessionID, domain.StatusActive).
		Where("revoked_at IS NULL AND expires_at > ?", refreshedAt.UTC()).
		Updates(map[string]any{"last_seen_at": refreshedAt.UTC(), "expires_at": expiresAt.UTC()})
	if result.Error != nil {
		return fmt.Errorf("update session expiry: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrUnauthenticated
	}
	return nil
}

// RevokeSession marks the active current session as revoked. The API never deletes session rows.
func (repository *GORMRepository) RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time, reason string) error {
	result := repository.database.WithContext(ctx).
		Model(&sessionModel{}).
		Where("id = ? AND status = ?", sessionID, domain.StatusActive).
		Where("revoked_at IS NULL").
		Updates(map[string]any{"status": "REVOKED", "revoked_at": revokedAt.UTC(), "revoke_reason": reason})
	if result.Error != nil {
		return fmt.Errorf("revoke session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrUnauthenticated
	}
	return nil
}

func (repository *GORMRepository) findRoles(ctx context.Context, tenantID, userID string, now time.Time) ([]domain.ReferenceName, error) {
	var rows []roleProjection
	result := repository.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Distinct("role.id", "role.name", "role.code").
		Select("role.id, role.name, role.code").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id AND application.code = ? AND application.status = ?", consoleApplicationCode, domain.StatusActive).
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", domain.StatusActive).
		Where("binding.tenant_id = ? AND binding.subject_type = ? AND binding.subject_id = ? AND binding.scope_type = ? AND binding.scope_id = ? AND binding.status = ?", tenantID, "USER", userID, "TENANT", "", domain.StatusActive).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now.UTC()).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now.UTC()).
		Order("role.code").
		Find(&rows)
	if result.Error != nil {
		return nil, fmt.Errorf("query principal roles: %w", result.Error)
	}
	roles := make([]domain.ReferenceName, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, domain.ReferenceName{ID: row.ID, Name: row.Name, Code: row.Code})
	}
	return roles, nil
}

func (repository *GORMRepository) findPermissionCodes(ctx context.Context, tenantID, userID string, now time.Time) ([]string, error) {
	var rows []permissionProjection
	result := repository.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Distinct("permission.code").
		Select("permission.code").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id AND application.code = ? AND application.status = ?", consoleApplicationCode, domain.StatusActive).
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", domain.StatusActive).
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = ?", "ALLOW").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = binding.tenant_id AND permission.application_id = binding.application_id AND permission.status = ?", domain.StatusActive).
		Where("binding.tenant_id = ? AND binding.subject_type = ? AND binding.subject_id = ? AND binding.scope_type = ? AND binding.scope_id = ? AND binding.status = ?", tenantID, "USER", userID, "TENANT", "", domain.StatusActive).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now.UTC()).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now.UTC()).
		Order("permission.code").
		Find(&rows)
	if result.Error != nil {
		return nil, fmt.Errorf("query principal permissions: %w", result.Error)
	}
	permissions := make([]string, 0, len(rows))
	for _, row := range rows {
		permissions = append(permissions, row.Code)
	}
	return permissions, nil
}
