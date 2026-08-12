// Package infrastructure provides GORM-backed identity repositories.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
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
		Where("account.username = ? AND account.auth_source = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", accountName, "LOCAL", time.Now().UTC()).
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

// FindLoginAccountByIdentityID resolves the durable identity binding used by
// OIDC login. It intentionally does not join password credentials: an OIDC
// account may be credential-free while still being a valid platform account.
func (repository *GORMRepository) FindLoginAccountByIdentityID(ctx context.Context, identityID string) (domain.LoginAccount, error) {
	var row loginAccountProjection
	result := repository.database.WithContext(ctx).
		Table("iam_account AS account").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code, tenant.status AS tenant_status,
			user.id AS user_id, user.display_name AS user_name, user.status AS user_status,
			account.id AS account_id, COALESCE(account.username, account.id) AS account_name, account.status AS account_status, account.locked_until,
			NULL AS password_hash, '' AS hash_algorithm, NULL AS algorithm_params,
			'' AS credential_status, NULL AS credential_expiry`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = account.tenant_id").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
		Where("user.id = ? AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", identityID, domain.StatusActive, time.Now().UTC()).
		Order("CASE WHEN account.auth_source = 'KEYCLOAK' THEN 0 ELSE 1 END").
		Limit(1).Find(&row)
	if result.Error != nil {
		return domain.LoginAccount{}, fmt.Errorf("query OIDC account: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.LoginAccount{}, application.ErrUnauthenticated
	}
	account := toDomainLoginAccount(row)
	account.CredentialStatus = domain.StatusActive
	return account, nil
}

// RecordSuccessfulPasswordVerification 在密码验证成功后锁定当前活动凭据并清除失败状态；
// 锁内再次检查凭据有效期，避免校验完成到状态落库之间凭据被停用或过期。
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

// CreateSession 以账号行的排他锁串行化同一账号的登录。清理过期会话、判断并发会话、
// 可选替换旧会话和插入新会话处于同一事务，两个终端不能同时越过活动会话检查。
func (repository *GORMRepository) CreateSession(ctx context.Context, account domain.LoginAccount, session domain.Session, idleTimeout time.Duration, replaceExisting bool) error {
	if idleTimeout <= 0 {
		return application.ErrUnauthenticated
	}
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		now := session.CreatedAt.UTC()
		idleCutoff := now.Add(-idleTimeout)
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
			Where("valid_until IS NULL OR valid_until > ?", now).
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

		absoluteTimeoutReason := "ABSOLUTE_TIMEOUT"
		if err := transaction.Model(&sessionModel{}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL AND expires_at <= ?", account.TenantID, account.AccountID, domain.StatusActive, now).
			Updates(map[string]any{"status": "EXPIRED", "revoked_at": now, "revoke_reason": absoluteTimeoutReason}).Error; err != nil {
			return fmt.Errorf("expire account sessions past absolute timeout: %w", err)
		}

		idleTimeoutReason := "IDLE_TIMEOUT"
		if err := transaction.Model(&sessionModel{}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL AND expires_at > ?", account.TenantID, account.AccountID, domain.StatusActive, now).
			Where("COALESCE(last_interactive_at, last_seen_at) <= ?", idleCutoff).
			Updates(map[string]any{"status": "EXPIRED", "revoked_at": now, "revoke_reason": idleTimeoutReason}).Error; err != nil {
			return fmt.Errorf("expire idle account sessions before login: %w", err)
		}

		var activeSessionCount int64
		if err := transaction.Model(&sessionModel{}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", account.TenantID, account.AccountID, domain.StatusActive).
			Where("expires_at > ? AND COALESCE(last_interactive_at, last_seen_at) > ?", now, idleCutoff).
			Count(&activeSessionCount).Error; err != nil {
			return fmt.Errorf("count active account sessions before login: %w", err)
		}
		if activeSessionCount > 0 && !replaceExisting {
			return application.ErrConcurrentSession
		}
		if activeSessionCount > 0 {
			if err := transaction.Model(&sessionModel{}).
				Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", account.TenantID, account.AccountID, domain.StatusActive).
				Updates(map[string]any{
					"status": "REVOKED", "revoked_at": now, "revoke_reason": "REPLACED_BY_PASSWORD_LOGIN",
				}).Error; err != nil {
				return fmt.Errorf("revoke existing account sessions before replacement login: %w", err)
			}
		}

		if err := transaction.Model(&accountModel{}).
			Where("id = ?", persistedAccount.ID).
			Updates(map[string]any{"last_login_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("update account successful login: %w", err)
		}

		userAgent := session.UserAgent
		if err := transaction.Create(&sessionModel{
			ID:                session.ID,
			TenantID:          session.TenantID,
			AccountID:         session.AccountID,
			IPAddress:         nullableBytes(session.IPAddress),
			UserAgent:         nullableString(&userAgent),
			CreatedAt:         now,
			LastSeenAt:        now,
			LastInteractiveAt: now,
			ExpiresAt:         session.ExpiresAt.UTC(),
			Status:            domain.StatusActive,
		}).Error; err != nil {
			return fmt.Errorf("insert browser session: %w", err)
		}
		return nil
	})
}

// FindPrincipalBySession 每次都以数据库当前状态验证会话、账号、用户和租户，再加载平台
// 应用的活动角色与权限；Cookie 中不缓存权限，因此停用主体或撤销角色无需等待 JWT 过期。
func (repository *GORMRepository) FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time, idleTimeout time.Duration) (domain.Principal, error) {
	if idleTimeout <= 0 {
		return domain.Principal{}, application.ErrUnauthenticated
	}
	now = now.UTC()
	idleCutoff := now.Add(-idleTimeout)

	var session sessionModel
	if err := repository.database.WithContext(ctx).
		Select("id", "tenant_id", "account_id", "last_seen_at", "last_interactive_at", "expires_at", "revoked_at", "status").
		Where("id = ?", sessionID).Take(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.Principal{}, application.ErrUnauthenticated
		}
		return domain.Principal{}, fmt.Errorf("query session activity: %w", err)
	}
	if session.Status != domain.StatusActive || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return domain.Principal{}, application.ErrUnauthenticated
	}
	if !session.LastInteractiveAt.After(idleCutoff) {
		if err := repository.RevokeAccountSessions(ctx, session.TenantID, session.AccountID, now, "IDLE_TIMEOUT"); err != nil {
			return domain.Principal{}, err
		}
		return domain.Principal{}, application.ErrUnauthenticated
	}
	var row principalProjection
	result := repository.database.WithContext(ctx).
		Table("iam_session AS session").
		Select(`session.id AS session_id, tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code,
			user.id AS user_id, user.display_name AS user_name, account.id AS account_id, COALESCE(account.username, account.id) AS account_name`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = session.tenant_id AND tenant.status = ?", domain.StatusActive).
		Joins("JOIN iam_account AS account ON account.id = session.account_id AND account.tenant_id = session.tenant_id AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", domain.StatusActive, now).
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = session.tenant_id AND user.status = ?", domain.StatusActive).
		Where("session.id = ? AND session.status = ?", sessionID, domain.StatusActive).
		Where("session.revoked_at IS NULL AND session.expires_at > ? AND session.last_interactive_at > ?", now, idleCutoff).
		Limit(1).
		Find(&row)
	if result.Error != nil {
		return domain.Principal{}, fmt.Errorf("query authenticated session: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.Principal{}, application.ErrUnauthenticated
	}
	touch := repository.database.WithContext(ctx).Model(&sessionModel{}).
		Where("id = ? AND status = ? AND revoked_at IS NULL AND expires_at > ? AND last_interactive_at > ?", sessionID, domain.StatusActive, now, idleCutoff).
		Update("last_seen_at", now)
	if touch.Error != nil {
		return domain.Principal{}, fmt.Errorf("touch authenticated session: %w", touch.Error)
	}
	if touch.RowsAffected == 0 {
		// MySQL reports zero changed rows when concurrent browser requests write the same
		// DATETIME(3) value. Re-check instead of treating that harmless no-op as a logout;
		// the extra read still rejects a session revoked concurrently with this request.
		var activeSession sessionModel
		check := repository.database.WithContext(ctx).
			Select("id").
			Where("id = ? AND status = ? AND revoked_at IS NULL AND expires_at > ? AND last_interactive_at > ?", sessionID, domain.StatusActive, now, idleCutoff).
			Take(&activeSession)
		if check.Error != nil {
			if errors.Is(check.Error, gorm.ErrRecordNotFound) {
				return domain.Principal{}, application.ErrUnauthenticated
			}
			return domain.Principal{}, fmt.Errorf("recheck authenticated session after touch: %w", check.Error)
		}
	}

	roles, err := repository.findRoles(ctx, row.TenantID, row.UserID, row.AccountID, now)
	if err != nil {
		return domain.Principal{}, err
	}
	permissions, err := repository.findPermissionCodes(ctx, row.TenantID, row.UserID, row.AccountID, now)
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

// RecordSessionInteraction records a browser event that was explicitly initiated by the user.
// Ordinary authenticated requests must not call this method: they only update last_seen_at in
// FindPrincipalBySession so background polling cannot keep an idle session alive.
func (repository *GORMRepository) RecordSessionInteraction(ctx context.Context, sessionID string, interactedAt time.Time, idleTimeout time.Duration) error {
	if idleTimeout <= 0 {
		return application.ErrUnauthenticated
	}
	interactedAt = interactedAt.UTC()
	idleCutoff := interactedAt.Add(-idleTimeout)
	result := repository.database.WithContext(ctx).
		Model(&sessionModel{}).
		Where("id = ? AND status = ? AND revoked_at IS NULL", sessionID, domain.StatusActive).
		Where("expires_at > ? AND last_interactive_at > ?", interactedAt, idleCutoff).
		Updates(map[string]any{"last_seen_at": interactedAt, "last_interactive_at": interactedAt})
	if result.Error != nil {
		return fmt.Errorf("record session interaction: %w", result.Error)
	}
	if result.RowsAffected != 0 {
		return nil
	}

	// A concurrent activity ping may write the same DATETIME(3) value. Verify that the session
	// remains usable before deciding whether a zero-row update is an actual expiry/revocation.
	var activeSession sessionModel
	check := repository.database.WithContext(ctx).
		Select("id").
		Where("id = ? AND status = ? AND revoked_at IS NULL AND expires_at > ? AND last_interactive_at > ?", sessionID, domain.StatusActive, interactedAt, idleCutoff).
		Take(&activeSession)
	if check.Error != nil {
		if errors.Is(check.Error, gorm.ErrRecordNotFound) {
			return application.ErrUnauthenticated
		}
		return fmt.Errorf("recheck session interaction: %w", check.Error)
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

// RevokeAccountSessions invalidates all active SSO sessions and every OIDC refresh-token family
// derived from them for one tenant account. It is used both for explicit global logout and for
// inactivity expiry discovered in any child system.
func (repository *GORMRepository) RevokeAccountSessions(ctx context.Context, tenantID, accountID string, revokedAt time.Time, reason string) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		revokedAt = revokedAt.UTC()
		// Lock every active browser session for this account. Authorization-code consumption locks
		// the same session row before creating a refresh family, so logout and code exchange are
		// serialized and neither can recreate an old user's grant after revocation.
		var lockedSessions []sessionModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", tenantID, accountID, domain.StatusActive).
			Find(&lockedSessions).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrUnauthenticated
			}
			return fmt.Errorf("lock account sessions before revocation: %w", err)
		}
		if len(lockedSessions) == 0 {
			return application.ErrUnauthenticated
		}
		// Revoke OAuth refresh-token families first while the session IDs are still available. This
		// prevents an RP from refreshing an old user's ID/access token after global logout.
		var activeFamilyIDs []string
		if err := transaction.Table("oauth_token_family").
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", tenantID, accountID, "ACTIVE").
			Pluck("id", &activeFamilyIDs).Error; err != nil {
			return fmt.Errorf("list account OAuth token families: %w", err)
		}
		if len(activeFamilyIDs) > 0 {
			if err := transaction.Table("oauth_refresh_token").
				Where("tenant_id = ? AND token_family_id IN ? AND revoked_at IS NULL", tenantID, activeFamilyIDs).
				Updates(map[string]any{"status": "REVOKED", "revoked_at": revokedAt, "revoke_reason": reason}).Error; err != nil {
				return fmt.Errorf("revoke account OAuth refresh tokens: %w", err)
			}
		}
		if err := transaction.Table("oauth_token_family").
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", tenantID, accountID, "ACTIVE").
			Updates(map[string]any{"status": "REVOKED", "revoked_at": revokedAt, "revoke_reason": reason}).Error; err != nil {
			return fmt.Errorf("revoke account OAuth token families: %w", err)
		}
		if err := transaction.Table("oauth_authorization_code").
			Where("tenant_id = ? AND account_id = ? AND status = ? AND consumed_at IS NULL", tenantID, accountID, "ACTIVE").
			Update("status", "REVOKED").Error; err != nil {
			return fmt.Errorf("revoke account OAuth authorization codes: %w", err)
		}
		if err := transaction.Model(&sessionModel{}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", tenantID, accountID, domain.StatusActive).
			Updates(map[string]any{"status": "REVOKED", "revoked_at": revokedAt, "revoke_reason": reason}).Error; err != nil {
			return fmt.Errorf("revoke account sessions: %w", err)
		}
		return nil
	})
}

func (repository *GORMRepository) findRoles(ctx context.Context, tenantID, userID, accountID string, now time.Time) ([]domain.ReferenceName, error) {
	var rows []roleProjection
	now = now.UTC()
	subjectSQL, subjectArgs := principalBindingSubjectFilter(userID, accountID, now)
	result := repository.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Distinct("role.id", "role.name", "role.code").
		Select("role.id, role.name, role.code").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id AND application.code = ? AND application.status = ?", consoleApplicationCode, domain.StatusActive).
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", domain.StatusActive).
		Where("binding.tenant_id = ? AND binding.scope_type = ? AND binding.scope_id = ? AND binding.status = ?", tenantID, "TENANT", "", domain.StatusActive).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where(subjectSQL, subjectArgs...).
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

func (repository *GORMRepository) findPermissionCodes(ctx context.Context, tenantID, userID, accountID string, now time.Time) ([]string, error) {
	var rows []permissionProjection
	now = now.UTC()
	subjectSQL, subjectArgs := principalBindingSubjectFilter(userID, accountID, now)
	result := repository.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Distinct("permission.code").
		Select("permission.code").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id AND application.code = ? AND application.status = ?", consoleApplicationCode, domain.StatusActive).
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", domain.StatusActive).
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = ?", "ALLOW").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = binding.tenant_id AND permission.application_id = binding.application_id AND permission.status = ?", domain.StatusActive).
		Where("binding.tenant_id = ? AND binding.scope_type = ? AND binding.scope_id = ? AND binding.status = ?", tenantID, "TENANT", "", domain.StatusActive).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where(subjectSQL, subjectArgs...).
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

// principalBindingSubjectFilter mirrors authorization checks when constructing the principal
// consumed by route middleware. It keeps USER, ACCOUNT, ORG_UNIT and POSITION bindings in one
// authorization model rather than making organization-derived roles display-only.
func principalBindingSubjectFilter(userID, accountID string, now time.Time) (string, []any) {
	return `(
		(binding.subject_type = ? AND binding.subject_id = ?)
		OR (binding.subject_type = ? AND binding.subject_id = ?)
		OR (
			binding.subject_type IN (?, ?)
			AND EXISTS (
				SELECT 1
				FROM iam_membership AS membership
				JOIN iam_org_unit AS organization
					ON organization.id = membership.org_unit_id
					AND organization.tenant_id = membership.tenant_id
					AND organization.status = ?
				JOIN iam_position AS position
					ON position.id = membership.position_id
					AND position.tenant_id = membership.tenant_id
					AND position.status = ?
				WHERE membership.tenant_id = binding.tenant_id
					AND membership.user_id = ?
					AND membership.status = ?
					AND membership.inherit_authorization = 1
					AND (membership.valid_from IS NULL OR membership.valid_from <= ?)
					AND (membership.valid_until IS NULL OR membership.valid_until > ?)
					AND (
						(binding.subject_type = ? AND membership.org_unit_id = binding.subject_id)
						OR (binding.subject_type = ? AND membership.position_id = binding.subject_id)
					)
			)
		)
	)`, []any{
			"USER", userID,
			"ACCOUNT", accountID,
			"ORG_UNIT", "POSITION",
			domain.StatusActive, domain.StatusActive,
			userID, domain.StatusActive, now, now,
			"ORG_UNIT", "POSITION",
		}
}
