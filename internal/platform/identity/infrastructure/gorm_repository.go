// Package infrastructure 提供基于 GORM 的身份仓储。
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const consoleApplicationCode = "platform"

// GORMRepository 在迁移所管的平台 schema 中持久化身份凭据、组织数据与浏览器会话状态；
// 它不使用 GORM 的 schema 迁移 API。
type GORMRepository struct {
	database *gorm.DB
}

// NewGORMRepository 使用应用共享的 GORM 句柄构造身份仓储；句柄为 nil 时返回错误。
func NewGORMRepository(database *gorm.DB) (*GORMRepository, error) {
	if database == nil {
		return nil, errors.New("identity GORM database must not be nil")
	}
	return &GORMRepository{database: database}, nil
}

// FindLoginAccount 按用户名返回本地账号与密码凭据。它有意不过滤账号状态：应用层需要
// 区分“锁定窗口内”与“通用认证失败”，且不能借此暴露其他账号状态。
func (repository *GORMRepository) FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error) {
	var row loginAccountProjection
	result := repository.database.WithContext(ctx).
		Table("iam_account AS account").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code, tenant.status AS tenant_status,
			user.id AS user_id, user.display_name AS user_name, user.status AS user_status,
			account.id AS account_id, COALESCE(account.username, account.id) AS account_name, account.status AS account_status, account.locked_until,
			credential.password_hash, credential.hash_algorithm, credential.algorithm_params,
			credential.status AS credential_status, credential.expires_at AS credential_expiry, credential.must_change AS must_change_password`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = account.tenant_id").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id AND (user.valid_until IS NULL OR user.valid_until > ?)", time.Now().UTC()).
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

// FindLoginAccountByIdentityID 解析 OIDC 登录使用的持久身份绑定。它有意不联接密码凭据：
// OIDC 账号可以没有本地凭据而仍是有效的平台账号。
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
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id AND (user.valid_until IS NULL OR user.valid_until > ?)", time.Now().UTC()).
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
		// 必须锁定"刚刚被验证过的那条凭据"：只按账号锁定活动凭据时，管理员在密码校验完成
		// 与会话创建之间重置密码，这里会锁定新凭据并顺利通过，旧密码的登录请求随即拿到会话。
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("account_id = ? AND status = ?", account.AccountID, domain.StatusActive).
			Where("password_hash = ?", account.PasswordHash).
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
			Where("id = iam_account.user_id AND tenant_id = iam_account.tenant_id AND status = ? AND (valid_until IS NULL OR valid_until > ?)", domain.StatusActive, now)
		activeTenant := transaction.Model(&tenantModel{}).
			Select("1").
			Where("id = iam_account.tenant_id AND status = ?", domain.StatusActive)

		var persistedAccount accountModel
		accountQuery := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("id = ? AND tenant_id = ? AND user_id = ? AND status = ?", account.AccountID, account.TenantID, account.UserID, domain.StatusActive).
			Where("locked_until IS NULL OR locked_until <= ?", now).
			Where("valid_until IS NULL OR valid_until > ?", now).
			Where("EXISTS (?)", activeUser).
			Where("EXISTS (?)", activeTenant)
		if len(account.PasswordHash) > 0 {
			// 会话必须绑定"刚刚验证过的密码"：校验完成到落库之间管理员重置密码时，
			// 旧密码请求不能凭借已过期的验证结果建立新会话。OIDC 登录没有本地密码，
			// 由 IdP 保证身份新鲜度，因此空哈希不追加该条件。
			verifiedCredential := transaction.Model(&passwordCredentialModel{}).
				Select("1").
				Where("account_id = iam_account.id AND status = ? AND password_hash = ?", domain.StatusActive, account.PasswordHash)
			accountQuery = accountQuery.Where("EXISTS (?)", verifiedCredential)
		}
		result := accountQuery.Limit(1).Find(&persistedAccount)
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
		Select(`session.id AS session_id, session.ip_address AS login_ip_address, tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code,
			user.id AS user_id, user.display_name AS user_name, account.id AS account_id, COALESCE(account.username, account.id) AS account_name`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = session.tenant_id AND tenant.status = ?", domain.StatusActive).
		Joins("JOIN iam_account AS account ON account.id = session.account_id AND account.tenant_id = session.tenant_id AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", domain.StatusActive, now).
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = session.tenant_id AND user.status = ? AND (user.valid_until IS NULL OR user.valid_until > ?)", domain.StatusActive, now).
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
		// MySQL 在并发浏览器请求写入相同 DATETIME(3) 值时报零变更行。这里复核而不是把无害
		// 的 no-op 当作登出；多出的读操作仍能拒绝与本请求并发被吊销的会话。
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
		LoginIP:         append(net.IP(nil), row.LoginIPAddress...),
		Tenant:          domain.ReferenceName{ID: row.TenantID, Name: row.TenantName, Code: row.TenantCode},
		User:            domain.ReferenceName{ID: row.UserID, Name: row.UserName},
		Account:         domain.ReferenceName{ID: row.AccountID, Name: row.AccountName},
		Roles:           roles,
		PermissionCodes: permissions,
	}, nil
}

// RefreshSession 保持登录时确定的不可变到期时间，只记录当前活动。返回持久化的到期时间，
// 让调用方签发的替换 Cookie/JWT 永远不会超出原始登录会话的生命周期。
func (repository *GORMRepository) RefreshSession(ctx context.Context, sessionID string, refreshedAt time.Time) (time.Time, error) {
	var session sessionModel
	lookup := repository.database.WithContext(ctx).
		Where("id = ? AND status = ? AND revoked_at IS NULL AND expires_at > ?", sessionID, domain.StatusActive, refreshedAt.UTC()).
		Take(&session)
	if lookup.Error != nil {
		if errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return time.Time{}, application.ErrUnauthenticated
		}
		return time.Time{}, fmt.Errorf("read fixed session expiry: %w", lookup.Error)
	}
	result := repository.database.WithContext(ctx).
		Model(&sessionModel{}).
		Where("id = ? AND status = ?", sessionID, domain.StatusActive).
		Where("revoked_at IS NULL AND expires_at > ?", refreshedAt.UTC()).
		Update("last_seen_at", refreshedAt.UTC())
	if result.Error != nil {
		return time.Time{}, fmt.Errorf("refresh fixed session activity: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return time.Time{}, application.ErrUnauthenticated
	}
	return session.ExpiresAt.UTC(), nil
}

// RecordSessionInteraction 只记录用户显式发起的浏览器事件。普通已认证请求不得调用本方法：
// 它们只在 FindPrincipalBySession 中更新 last_seen_at，使后台轮询无法为空闲会话续命。
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

	// 并发的活动 ping 可能写入相同的 DATETIME(3) 值。在把零行更新判定为真正过期/吊销之前，
	// 先复核会话是否仍可用。
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

// RevokeSession 把当前活跃会话标记为已吊销。API 从不删除会话行。
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

// RevokeAccountSessions 使一个租户账号的全部活跃 SSO 会话及由其派生的每个 OIDC 刷新
// 令牌族失效。它同时服务于显式全局登出，以及任一子系统发现的不活跃过期。
func (repository *GORMRepository) RevokeAccountSessions(ctx context.Context, tenantID, accountID string, revokedAt time.Time, reason string) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		revokedAt = revokedAt.UTC()
		// 锁定该账号的全部活跃浏览器会话。授权码消费在创建刷新令牌族之前锁定同一会话行，
		// 因此登出与码交换被串行化，二者都不可能在吊销后重建旧用户的授权。
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
		// 趁会话 ID 仍可用，先吊销 OAuth 刷新令牌族，防止 RP 在全局登出后为旧用户刷新
		// ID/访问令牌。
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
		// Outbox 与本地会话撤销共用当前事务：只有两者都成功才提交。投递任务来源仅限
		// 已建立 Token Family 且登记了 ACTIVE Back-Channel URI 的 RP，授权码尚未换取
		// Token 时 RP 并不存在可注销会话，因此不能提前通知。
		if err := repository.enqueueBackchannelLogout(transaction, tenantID, accountID, lockedSessions, revokedAt); err != nil {
			return err
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

type backchannelLogoutTarget struct {
	OAuthClientID string `gorm:"column:oauth_client_id"`
	SessionID     string `gorm:"column:session_id"`
	UserID        string `gorm:"column:user_id"`
}

// enqueueBackchannelLogout 在调用方事务中为已登录 RP 创建持久投递任务。
// 参数 transaction 必须是 RevokeAccountSessions 的事务句柄；返回错误时调用方必须回滚整个会话撤销。
func (repository *GORMRepository) enqueueBackchannelLogout(transaction *gorm.DB, tenantID, accountID string, sessions []sessionModel, now time.Time) error {
	if transaction == nil || len(sessions) == 0 {
		return nil
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	var targets []backchannelLogoutTarget
	if err := transaction.Table("oauth_token_family AS family").
		Distinct("family.oauth_client_id", "family.session_id", "family.user_id").
		Joins("JOIN platform_oauth_client AS client ON client.id = family.oauth_client_id AND client.tenant_id = family.tenant_id AND client.status = ?", domain.StatusActive).
		Joins("JOIN platform_oauth_backchannel_logout_uri AS uri ON uri.oauth_client_id = family.oauth_client_id").
		Where("family.tenant_id = ? AND family.account_id = ? AND family.session_id IN ?", tenantID, accountID, sessionIDs).
		Find(&targets).Error; err != nil {
		return fmt.Errorf("list OIDC back-channel logout targets: %w", err)
	}
	for _, target := range targets {
		id, err := ulid.New(now)
		if err != nil {
			return fmt.Errorf("generate back-channel logout event ID: %w", err)
		}
		row := map[string]any{
			"id": id, "oauth_client_id": target.OAuthClientID, "session_id": target.SessionID,
			"subject_id": target.UserID, "jti": id, "status": "PENDING", "attempt_count": 0,
			"next_attempt_at": now.UTC(), "created_at": now.UTC(),
		}
		// 唯一键 (oauth_client_id, session_id) 保证重试撤销不会产生第二条通知；其他
		// 数据库错误仍向上传播，不能把 Outbox 失败伪装成注销成功。
		if err = transaction.Table("platform_oauth_backchannel_logout_outbox").
			Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "oauth_client_id"}, {Name: "session_id"}}, DoNothing: true}).
			Create(row).Error; err != nil {
			return fmt.Errorf("enqueue OIDC back-channel logout: %w", err)
		}
	}
	return nil
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

// principalBindingSubjectFilter 在构造路由中间件消费的主体时，与授权检查保持同一模型。
// 它把 USER、ACCOUNT、ORG_UNIT 和 POSITION 绑定放进同一授权模型，而不是让组织派生角色
// 仅作展示。
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
