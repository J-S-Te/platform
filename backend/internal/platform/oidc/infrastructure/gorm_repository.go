// Package infrastructure persists OIDC/OAuth runtime state with GORM and MySQL transactions.
package infrastructure

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const activeStatus = "ACTIVE"

type Repository struct{ database *gorm.DB }

// NewRepository creates the MySQL-backed OIDC/OAuth runtime repository.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("OIDC/OAuth database must not be nil")
	}
	return &Repository{database: database}, nil
}

type clientRow struct {
	ID                     string
	TenantID               string
	ClientID               string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
}

type credentialRow struct {
	SecretHash []byte
	ValidUntil *time.Time
}

type stringRow struct{ Value string }

type clientJWKRow struct {
	PublicJWK  []byte
	KeyID      string
	ValidUntil *time.Time
}

type authorizationCodeRow struct {
	ID                  string
	TenantID            string
	OAuthClientID       string `gorm:"column:oauth_client_id"`
	SessionID           string
	AccountID           string
	UserID              string
	CodeHash            []byte
	RedirectURI         string
	Scope               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
	Status              string
}

type tokenFamilyRow struct {
	ID            string
	TenantID      string
	OAuthClientID string `gorm:"column:oauth_client_id"`
	SessionID     string
	AccountID     string
	UserID        string
	Scope         string
	AuthorizedAt  time.Time
	CreatedAt     time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokeReason  string
	Status        string
}

type refreshTokenRow struct {
	ID                   string
	TenantID             string
	OAuthClientID        string `gorm:"column:oauth_client_id"`
	TokenFamilyID        string
	ParentRefreshTokenID *string
	TokenHash            []byte
	IssuedAt             time.Time
	ExpiresAt            time.Time
	UsedAt               *time.Time
	RevokedAt            *time.Time
	RevokeReason         string
	Status               string
}

type refreshProjection struct {
	ID                   string
	TenantID             string
	OAuthClientID        string `gorm:"column:oauth_client_id"`
	TokenFamilyID        string
	ParentRefreshTokenID *string
	TokenHash            []byte
	IssuedAt             time.Time
	ExpiresAt            time.Time
	UsedAt               *time.Time
	RevokedAt            *time.Time
	RevokeReason         string
	Status               string
	SessionID            string
	AccountID            string
	UserID               string
	Scope                string
	AuthorizedAt         time.Time
	FamilyExpiresAt      time.Time
	FamilyRevokedAt      *time.Time
	FamilyStatus         string
}

type tokenRevocationRow struct {
	ID            string
	TenantID      string
	OAuthClientID string `gorm:"column:oauth_client_id"`
	TokenHash     []byte
	TokenType     string
	RevokedAt     time.Time
	ExpiresAt     *time.Time
	Reason        string
}

type userInfoProjection struct {
	TenantID          string
	OAuthClientID     string `gorm:"column:oauth_client_id"`
	SessionID         string
	UserID            string
	DisplayName       string
	PreferredUsername string
	Email             string
}

// FindClient resolves one active registration and its exact redirect URI, grants, scopes, and
// active client-secret verifiers. It also rejects clients whose tenant, application, or
// environment is disabled.
func (r *Repository) FindClient(ctx context.Context, clientID string, now time.Time) (domain.Client, error) {
	client, err := r.findClient(ctx, r.database, "platform_oauth_client.client_id = ?", clientID)
	if err != nil {
		return domain.Client{}, mapError(err)
	}
	return r.withCapabilities(ctx, r.database, client, now)
}

func (r *Repository) findClient(ctx context.Context, database *gorm.DB, condition string, value any) (clientRow, error) {
	var client clientRow
	err := database.WithContext(ctx).Table("platform_oauth_client").
		Select("platform_oauth_client.id, platform_oauth_client.tenant_id, platform_oauth_client.client_id, platform_oauth_client.client_type, platform_oauth_client.token_auth_method, platform_oauth_client.access_token_ttl_seconds, platform_oauth_client.refresh_token_ttl_seconds, platform_oauth_client.require_pkce").
		Joins("JOIN iam_tenant ON iam_tenant.id = platform_oauth_client.tenant_id AND iam_tenant.status = ?", activeStatus).
		Joins("JOIN platform_application ON platform_application.id = platform_oauth_client.application_id AND platform_application.status = ?", activeStatus).
		Joins("JOIN platform_application_environment ON platform_application_environment.id = platform_oauth_client.environment_id AND platform_application_environment.status = ?", activeStatus).
		Where(condition+" AND platform_oauth_client.status = ?", value, activeStatus).
		Take(&client).Error
	return client, err
}

func (r *Repository) withCapabilities(ctx context.Context, database *gorm.DB, client clientRow, now time.Time) (domain.Client, error) {
	var redirects, grants, scopes []stringRow
	if err := database.WithContext(ctx).Table("platform_oauth_redirect_uri").Select("redirect_uri AS value").Where("oauth_client_id = ?", client.ID).Find(&redirects).Error; err != nil {
		return domain.Client{}, err
	}
	if err := database.WithContext(ctx).Table("platform_oauth_grant_type").Select("grant_type AS value").Where("oauth_client_id = ?", client.ID).Find(&grants).Error; err != nil {
		return domain.Client{}, err
	}
	if err := database.WithContext(ctx).Table("platform_oauth_client_scope").Select("scope_code AS value").Where("oauth_client_id = ?", client.ID).Find(&scopes).Error; err != nil {
		return domain.Client{}, err
	}
	var keys []clientJWKRow
	if err := database.WithContext(ctx).Table("oauth_client_jwk").
		Select("public_jwk, key_id, valid_until").
		Where("oauth_client_id = ? AND status = ? AND revoked_at IS NULL AND valid_from <= ? AND (valid_until IS NULL OR valid_until > ?)", client.ID, activeStatus, now, now).
		Find(&keys).Error; err != nil {
		return domain.Client{}, err
	}
	var credentials []credentialRow
	if err := database.WithContext(ctx).Table("platform_oauth_client_credential").
		Select("secret_hash, valid_until").
		Where("oauth_client_id = ? AND credential_type = ? AND status = ? AND revoked_at IS NULL AND valid_from <= ? AND (valid_until IS NULL OR valid_until > ?)", client.ID, "secret", activeStatus, now, now).
		Find(&credentials).Error; err != nil {
		return domain.Client{}, err
	}
	result := domain.Client{
		ID: client.ID, TenantID: client.TenantID, ClientID: client.ClientID, ClientType: client.ClientType,
		TokenAuthMethod: client.TokenAuthMethod, AccessTokenTTLSeconds: client.AccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: client.RefreshTokenTTLSeconds, RequirePKCE: client.RequirePKCE,
		RedirectURIs: make(map[string]struct{}, len(redirects)), GrantTypes: make(map[string]struct{}, len(grants)),
		Scopes: make(map[string]struct{}, len(scopes)), Credentials: make([]domain.ClientCredential, 0, len(credentials)),
		JWKs: make([]domain.ClientJWK, 0, len(keys)),
	}
	for _, row := range redirects {
		result.RedirectURIs[row.Value] = struct{}{}
	}
	for _, row := range grants {
		result.GrantTypes[row.Value] = struct{}{}
	}
	for _, row := range scopes {
		result.Scopes[row.Value] = struct{}{}
	}
	for _, row := range credentials {
		result.Credentials = append(result.Credentials, domain.ClientCredential{SecretHash: append([]byte(nil), row.SecretHash...), ValidUntil: utcPointer(row.ValidUntil)})
	}
	for _, row := range keys {
		result.JWKs = append(result.JWKs, domain.ClientJWK{PublicJWK: append([]byte(nil), row.PublicJWK...), KeyID: row.KeyID, ValidUntil: utcPointer(row.ValidUntil)})
	}
	return result, nil
}

// ResolveSessionSubject returns a subject only while the local session and all tenant-scoped
// identity records remain active and the session is neither expired nor revoked.
func (r *Repository) ResolveSessionSubject(ctx context.Context, sessionID string, now time.Time) (domain.SessionSubject, error) {
	return r.resolveSessionSubject(ctx, r.database, sessionID, now, false)
}

func (r *Repository) resolveSessionSubject(ctx context.Context, database *gorm.DB, sessionID string, now time.Time, lock bool) (domain.SessionSubject, error) {
	var row struct {
		TenantID, SessionID, AccountID, UserID string
		ExpiresAt                              time.Time
	}
	query := database.WithContext(ctx).Table("iam_session").
		Select("iam_session.tenant_id, iam_session.id AS session_id, iam_session.account_id, iam_account.user_id, iam_session.expires_at").
		Joins("JOIN iam_tenant ON iam_tenant.id = iam_session.tenant_id AND iam_tenant.status = ?", activeStatus).
		Joins("JOIN iam_account ON iam_account.id = iam_session.account_id AND iam_account.tenant_id = iam_session.tenant_id AND iam_account.status = ? AND (iam_account.valid_until IS NULL OR iam_account.valid_until > ?)", activeStatus, now).
		Joins("JOIN iam_user ON iam_user.id = iam_account.user_id AND iam_user.tenant_id = iam_session.tenant_id AND iam_user.status = ?", activeStatus).
		Where("iam_session.id = ? AND iam_session.status = ? AND iam_session.revoked_at IS NULL AND iam_session.expires_at > ?", sessionID, activeStatus, now)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := query.Take(&row).Error
	if err != nil {
		return domain.SessionSubject{}, mapError(err)
	}
	return domain.SessionSubject{TenantID: row.TenantID, SessionID: row.SessionID, AccountID: row.AccountID, UserID: row.UserID, ExpiresAt: row.ExpiresAt.UTC()}, nil
}

func (r *Repository) CreateAuthorizationCode(ctx context.Context, code domain.AuthorizationCode) error {
	return r.database.WithContext(ctx).Table("oauth_authorization_code").Create(&authorizationCodeRow{
		ID: code.ID, TenantID: code.TenantID, OAuthClientID: code.OAuthClientID, SessionID: code.SessionID, AccountID: code.AccountID, UserID: code.UserID,
		CodeHash: code.CodeHash[:], RedirectURI: code.RedirectURI, Scope: joinScopes(code.Scopes), Nonce: code.Nonce, CodeChallenge: code.CodeChallenge,
		CodeChallengeMethod: code.CodeChallengeMethod, CreatedAt: code.CreatedAt.UTC(), ExpiresAt: code.ExpiresAt.UTC(), ConsumedAt: utcPointer(code.ConsumedAt), Status: code.Status,
	}).Error
}

func (r *Repository) FindAuthorizationCode(ctx context.Context, codeHash [32]byte, _ time.Time) (domain.AuthorizationCode, error) {
	var row authorizationCodeRow
	if err := r.database.WithContext(ctx).Table("oauth_authorization_code").Where("code_hash = ?", codeHash[:]).Take(&row).Error; err != nil {
		return domain.AuthorizationCode{}, mapError(err)
	}
	return authorizationCodeFromRow(row), nil
}

// ConsumeAuthorizationCode 锁定授权码摘要行，在事务内重做客户端、回调地址、授权模式和会话校验，
// 并原子地消费授权码、创建首个刷新令牌族节点。应用层的预校验不能替代这里的最终提交判断。
func (r *Repository) ConsumeAuthorizationCode(ctx context.Context, command application.ConsumeAuthorizationCodeCommand, now time.Time) (domain.TokenGrant, error) {
	var grant domain.TokenGrant
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var code authorizationCodeRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("oauth_authorization_code").Where("code_hash = ?", command.CodeHash[:]).Take(&code).Error; err != nil {
			return mapError(err)
		}
		if code.Status != domain.AuthorizationCodeStatusActive || code.ConsumedAt != nil || !code.ExpiresAt.After(now) || code.RedirectURI != command.RedirectURI {
			return application.ErrAuthorizationCodeUnavailable
		}
		client, err := r.findClient(ctx, tx, "platform_oauth_client.id = ?", command.ClientID)
		if err != nil {
			return mapError(err)
		}
		if code.TenantID != client.TenantID || code.OAuthClientID != client.ID || !r.hasGrant(ctx, tx, client.ID, "authorization_code") {
			return application.ErrAuthorizationCodeUnavailable
		}
		// 会话行与授权码一起在事务内复核并加锁，关闭“应用层预校验后用户退出，随后仍创建
		// 刷新令牌族”的竞态窗口。
		subject, err := r.resolveSessionSubject(ctx, tx, code.SessionID, now, true)
		if err != nil || subject.TenantID != code.TenantID || subject.SessionID != code.SessionID ||
			subject.AccountID != code.AccountID || subject.UserID != code.UserID {
			return application.ErrAuthorizationCodeUnavailable
		}
		if err := tx.Table("oauth_authorization_code").Where("id = ? AND status = ? AND consumed_at IS NULL", code.ID, domain.AuthorizationCodeStatusActive).
			Updates(map[string]any{"status": domain.AuthorizationCodeStatusConsumed, "consumed_at": now.UTC()}).Error; err != nil {
			return err
		}
		grant = grantFromCode(code, client.ClientID)
		if command.Refresh != nil {
			if err := validateInitialRefresh(*command.Refresh, grant); err != nil {
				return err
			}
			if err := r.createRefreshFamily(ctx, tx, *command.Refresh); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return domain.TokenGrant{}, err
	}
	return grant, nil
}

func (r *Repository) FindRefreshToken(ctx context.Context, tokenHash [32]byte, now time.Time) (domain.RefreshToken, error) {
	row, err := r.refreshByHash(ctx, r.database, tokenHash, false)
	if err != nil {
		return domain.RefreshToken{}, mapError(err)
	}
	return refreshFromProjection(row, now), nil
}

// RotateRefreshToken 在锁内把当前节点标记为已消费并插入唯一后继。任意已消费节点被再次提交时，
// 会先撤销整个令牌族再返回重放错误，使并发刷新和窃取后的重放都不能留下可用后代。
func (r *Repository) RotateRefreshToken(ctx context.Context, command application.RotateRefreshTokenCommand, now time.Time) (domain.TokenGrant, error) {
	var grant domain.TokenGrant
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := r.refreshByHash(ctx, tx, command.TokenHash, true)
		if err != nil {
			return mapError(err)
		}
		if row.OAuthClientID != command.ClientID || row.TenantID != command.Refresh.TenantID {
			return application.ErrNotFound
		}
		var family tokenFamilyRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("oauth_token_family").Where("id = ? AND tenant_id = ?", row.TokenFamilyID, row.TenantID).Take(&family).Error; err != nil {
			return mapError(err)
		}
		if row.Status == domain.RefreshTokenStatusConsumed || row.UsedAt != nil {
			// 必须在持有 family 行锁时撤销，确保另一实例不能同时从后代节点继续轮换。
			if err := revokeFamily(tx, family, now, "refresh_token_replay"); err != nil {
				return err
			}
			return application.ErrRefreshTokenReplay
		}
		if row.Status != domain.RefreshTokenStatusActive || row.RevokedAt != nil || !row.ExpiresAt.After(now) || family.Status != domain.TokenFamilyStatusActive || family.RevokedAt != nil || !family.ExpiresAt.After(now) {
			return application.ErrNotFound
		}
		client, err := r.findClient(ctx, tx, "platform_oauth_client.id = ?", command.ClientID)
		if err != nil {
			return mapError(err)
		}
		if !r.hasGrant(ctx, tx, client.ID, "refresh_token") {
			return application.ErrNotFound
		}
		grant = grantFromFamily(family, client.ClientID)
		if err := validateRotatedRefresh(command.Refresh, row, family, grant); err != nil {
			return err
		}
		if err := tx.Table("oauth_refresh_token").Where("id = ? AND status = ? AND used_at IS NULL", row.ID, domain.RefreshTokenStatusActive).
			Updates(map[string]any{"status": domain.RefreshTokenStatusConsumed, "used_at": now.UTC()}).Error; err != nil {
			return err
		}
		if err := tx.Table("oauth_refresh_token").Create(&refreshTokenRow{
			ID: command.Refresh.ID, TenantID: family.TenantID, OAuthClientID: family.OAuthClientID, TokenFamilyID: family.ID, ParentRefreshTokenID: stringPointer(row.ID),
			TokenHash: command.Refresh.TokenHash[:], IssuedAt: command.Refresh.IssuedAt.UTC(), ExpiresAt: command.Refresh.ExpiresAt.UTC(), Status: domain.RefreshTokenStatusActive,
		}).Error; err != nil {
			return err
		}
		return tx.Table("oauth_token_family").Where("id = ?").Update("expires_at", command.Refresh.ExpiresAt.UTC()).Error
	})
	if err != nil {
		return domain.TokenGrant{}, err
	}
	return grant, nil
}

// RevokeToken records access-token revocation or revokes the entire matching refresh-token family.
// Unknown refresh tokens are accepted as no-op to preserve RFC 7009 response behavior.
func (r *Repository) RevokeToken(ctx context.Context, command application.RevokeTokenCommand, now time.Time) error {
	return r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if command.TokenType == domain.TokenTypeRefresh {
			row, err := r.refreshByHash(ctx, tx, command.TokenHash, true)
			if err == nil && row.TenantID == command.TenantID && row.OAuthClientID == command.OAuthClientID {
				var family tokenFamilyRow
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("oauth_token_family").Where("id = ? AND tenant_id = ?", row.TokenFamilyID, row.TenantID).Take(&family).Error; err != nil {
					return mapError(err)
				}
				if err := revokeFamily(tx, family, now, command.Reason); err != nil {
					return err
				}
				return r.upsertRevocation(tx, command, now)
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return nil
		}
		return r.upsertRevocation(tx, command, now)
	})
}

func (r *Repository) upsertRevocation(tx *gorm.DB, command application.RevokeTokenCommand, now time.Time) error {
	return tx.Table("oauth_token_revocation").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "token_hash"}},
		DoUpdates: clause.Assignments(map[string]any{"oauth_client_id": command.OAuthClientID, "token_type": command.TokenType, "revoked_at": now.UTC(), "expires_at": utcPointer(command.ExpiresAt), "reason": command.Reason}),
	}).Create(&tokenRevocationRow{ID: command.RevocationID, TenantID: command.TenantID, OAuthClientID: command.OAuthClientID, TokenHash: command.TokenHash[:], TokenType: command.TokenType, RevokedAt: now.UTC(), ExpiresAt: utcPointer(command.ExpiresAt), Reason: command.Reason}).Error
}

func (r *Repository) IsTokenRevoked(ctx context.Context, tenantID string, tokenHash [32]byte, now time.Time) (bool, error) {
	var count int64
	err := r.database.WithContext(ctx).Table("oauth_token_revocation").Where("tenant_id = ? AND token_hash = ? AND (expires_at IS NULL OR expires_at > ?)", tenantID, tokenHash[:], now).Count(&count).Error
	return count > 0, err
}

func (r *Repository) ResolveUserInfo(ctx context.Context, query application.UserInfoQuery, now time.Time) (domain.UserInfoSubject, error) {
	var row userInfoProjection
	err := r.database.WithContext(ctx).Table("iam_session").
		Select("iam_session.tenant_id, platform_oauth_client.id AS oauth_client_id, iam_session.id AS session_id, iam_user.id AS user_id, iam_user.display_name, iam_account.username AS preferred_username, iam_user.email").
		Joins("JOIN iam_tenant ON iam_tenant.id = iam_session.tenant_id AND iam_tenant.status = ?", activeStatus).
		Joins("JOIN iam_account ON iam_account.id = iam_session.account_id AND iam_account.tenant_id = iam_session.tenant_id AND iam_account.status = ? AND (iam_account.valid_until IS NULL OR iam_account.valid_until > ?)", activeStatus, now).
		Joins("JOIN iam_user ON iam_user.id = iam_account.user_id AND iam_user.tenant_id = iam_session.tenant_id AND iam_user.status = ?", activeStatus).
		Joins("JOIN platform_oauth_client ON platform_oauth_client.id = ? AND platform_oauth_client.tenant_id = iam_session.tenant_id AND platform_oauth_client.status = ?", query.OAuthClientID, activeStatus).
		Where("iam_session.id = ? AND iam_session.tenant_id = ? AND iam_user.id = ? AND iam_session.status = ? AND iam_session.revoked_at IS NULL AND iam_session.expires_at > ?", query.SessionID, query.TenantID, query.UserID, activeStatus, now).
		Take(&row).Error
	if err != nil {
		return domain.UserInfoSubject{}, mapError(err)
	}
	return domain.UserInfoSubject{TenantID: row.TenantID, OAuthClientID: row.OAuthClientID, SessionID: row.SessionID, UserID: row.UserID, DisplayName: row.DisplayName, PreferredUsername: row.PreferredUsername, Email: row.Email}, nil
}

func (r *Repository) refreshByHash(ctx context.Context, database *gorm.DB, hash [32]byte, lock bool) (refreshProjection, error) {
	query := database.WithContext(ctx).Table("oauth_refresh_token").
		Select("oauth_refresh_token.id, oauth_refresh_token.tenant_id, oauth_refresh_token.oauth_client_id, oauth_refresh_token.token_family_id, oauth_refresh_token.parent_refresh_token_id, oauth_refresh_token.token_hash, oauth_refresh_token.issued_at, oauth_refresh_token.expires_at, oauth_refresh_token.used_at, oauth_refresh_token.revoked_at, oauth_refresh_token.revoke_reason, oauth_refresh_token.status, oauth_token_family.session_id, oauth_token_family.account_id, oauth_token_family.user_id, oauth_token_family.scope, oauth_token_family.authorized_at, oauth_token_family.expires_at AS family_expires_at, oauth_token_family.revoked_at AS family_revoked_at, oauth_token_family.status AS family_status").
		Joins("JOIN oauth_token_family ON oauth_token_family.id = oauth_refresh_token.token_family_id AND oauth_token_family.tenant_id = oauth_refresh_token.tenant_id").
		Where("oauth_refresh_token.token_hash = ?", hash[:])
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row refreshProjection
	return row, query.Take(&row).Error
}

func (r *Repository) createRefreshFamily(ctx context.Context, tx *gorm.DB, refresh application.NewRefreshToken) error {
	if err := tx.WithContext(ctx).Table("oauth_token_family").Create(&tokenFamilyRow{
		ID: refresh.TokenFamilyID, TenantID: refresh.TenantID, OAuthClientID: refresh.OAuthClientID, SessionID: refresh.SessionID, AccountID: refresh.AccountID, UserID: refresh.UserID,
		Scope: joinScopes(refresh.Scopes), AuthorizedAt: refresh.AuthorizedAt.UTC(), CreatedAt: refresh.IssuedAt.UTC(), ExpiresAt: refresh.ExpiresAt.UTC(), Status: domain.TokenFamilyStatusActive,
	}).Error; err != nil {
		return err
	}
	return tx.WithContext(ctx).Table("oauth_refresh_token").Create(&refreshTokenRow{
		ID: refresh.ID, TenantID: refresh.TenantID, OAuthClientID: refresh.OAuthClientID, TokenFamilyID: refresh.TokenFamilyID, TokenHash: refresh.TokenHash[:],
		IssuedAt: refresh.IssuedAt.UTC(), ExpiresAt: refresh.ExpiresAt.UTC(), Status: domain.RefreshTokenStatusActive,
	}).Error
}

func (r *Repository) hasGrant(ctx context.Context, database *gorm.DB, clientID, grant string) bool {
	var count int64
	return database.WithContext(ctx).Table("platform_oauth_grant_type").Where("oauth_client_id = ? AND grant_type = ?", clientID, grant).Count(&count).Error == nil && count == 1
}

func revokeFamily(database *gorm.DB, family tokenFamilyRow, now time.Time, reason string) error {
	updates := map[string]any{"status": domain.TokenFamilyStatusRevoked, "revoked_at": now.UTC(), "revoke_reason": reason}
	if err := database.Table("oauth_token_family").Where("id = ? AND tenant_id = ?", family.ID, family.TenantID).Updates(updates).Error; err != nil {
		return err
	}
	return database.Table("oauth_refresh_token").Where("token_family_id = ? AND tenant_id = ? AND revoked_at IS NULL", family.ID, family.TenantID).
		Updates(map[string]any{"status": domain.RefreshTokenStatusRevoked, "revoked_at": now.UTC(), "revoke_reason": reason}).Error
}

func validateInitialRefresh(refresh application.NewRefreshToken, grant domain.TokenGrant) error {
	if refresh.ID == "" || refresh.TokenFamilyID == "" || refresh.ParentRefreshTokenID != "" || refresh.TenantID != grant.TenantID || refresh.OAuthClientID != grant.OAuthClientID || refresh.SessionID != grant.SessionID || refresh.AccountID != grant.AccountID || refresh.UserID != grant.UserID || !refresh.AuthorizedAt.Equal(grant.AuthorizedAt) || !sameScopes(refresh.Scopes, grant.Scopes) || !refresh.ExpiresAt.After(refresh.IssuedAt) {
		return application.ErrInvalidGrant
	}
	return nil
}

func validateRotatedRefresh(refresh application.NewRefreshToken, parent refreshProjection, family tokenFamilyRow, grant domain.TokenGrant) error {
	if refresh.ID == "" || refresh.TokenFamilyID != "" || refresh.ParentRefreshTokenID != parent.ID || refresh.TenantID != family.TenantID || refresh.OAuthClientID != family.OAuthClientID || refresh.SessionID != family.SessionID || refresh.AccountID != family.AccountID || refresh.UserID != family.UserID || !refresh.AuthorizedAt.Equal(family.AuthorizedAt) || !sameScopes(refresh.Scopes, parseScopes(family.Scope)) || !refresh.ExpiresAt.After(refresh.IssuedAt) || !sameScopes(refresh.Scopes, grant.Scopes) {
		return application.ErrInvalidGrant
	}
	return nil
}

func authorizationCodeFromRow(row authorizationCodeRow) domain.AuthorizationCode {
	var hash [32]byte
	copy(hash[:], row.CodeHash)
	return domain.AuthorizationCode{ID: row.ID, TenantID: row.TenantID, OAuthClientID: row.OAuthClientID, SessionID: row.SessionID, AccountID: row.AccountID, UserID: row.UserID, CodeHash: hash, RedirectURI: row.RedirectURI, Scopes: parseScopes(row.Scope), Nonce: row.Nonce, CodeChallenge: row.CodeChallenge, CodeChallengeMethod: row.CodeChallengeMethod, CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(), ConsumedAt: utcPointer(row.ConsumedAt), Status: row.Status}
}

func refreshFromProjection(row refreshProjection, now time.Time) domain.RefreshToken {
	var hash [32]byte
	copy(hash[:], row.TokenHash)
	status := row.Status
	revokedAt := utcPointer(row.RevokedAt)
	revokeReason := row.RevokeReason
	if row.FamilyStatus != domain.TokenFamilyStatusActive || row.FamilyRevokedAt != nil || !row.FamilyExpiresAt.After(now.UTC()) {
		status = domain.RefreshTokenStatusRevoked
		if revokedAt == nil {
			revokedAt = utcPointer(row.FamilyRevokedAt)
		}
		if revokeReason == "" {
			revokeReason = "TOKEN_FAMILY_INACTIVE"
		}
	}
	return domain.RefreshToken{ID: row.ID, TenantID: row.TenantID, OAuthClientID: row.OAuthClientID, SessionID: row.SessionID, AccountID: row.AccountID, UserID: row.UserID, Scopes: parseScopes(row.Scope), AuthorizedAt: row.AuthorizedAt.UTC(), TokenFamilyID: row.TokenFamilyID, ParentRefreshTokenID: dereference(row.ParentRefreshTokenID), TokenHash: hash, IssuedAt: row.IssuedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(), UsedAt: utcPointer(row.UsedAt), RevokedAt: revokedAt, RevokeReason: revokeReason, Status: status}
}

func grantFromCode(code authorizationCodeRow, clientID string) domain.TokenGrant {
	return domain.TokenGrant{TenantID: code.TenantID, OAuthClientID: code.OAuthClientID, ClientID: clientID, SessionID: code.SessionID, AccountID: code.AccountID, UserID: code.UserID, Scopes: parseScopes(code.Scope), Nonce: code.Nonce, AuthorizedAt: code.CreatedAt.UTC()}
}

func grantFromFamily(family tokenFamilyRow, clientID string) domain.TokenGrant {
	return domain.TokenGrant{TenantID: family.TenantID, OAuthClientID: family.OAuthClientID, ClientID: clientID, SessionID: family.SessionID, AccountID: family.AccountID, UserID: family.UserID, Scopes: parseScopes(family.Scope), AuthorizedAt: family.AuthorizedAt.UTC()}
}

func joinScopes(scopes []string) string {
	return strings.Join(parseScopes(strings.Join(scopes, " ")), " ")
}
func parseScopes(scope string) []string {
	fields := strings.Fields(scope)
	sort.Strings(fields)
	if len(fields) < 2 {
		return fields
	}

	canonical := fields[:1]
	for _, scope := range fields[1:] {
		if scope != canonical[len(canonical)-1] {
			canonical = append(canonical, scope)
		}
	}
	return canonical
}
func sameScopes(left, right []string) bool {
	left, right = parseScopes(strings.Join(left, " ")), parseScopes(strings.Join(right, " "))
	return strings.Join(left, " ") == strings.Join(right, " ")
}
func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
func stringPointer(value string) *string { return &value }
func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	return err
}

var _ application.Repository = (*Repository)(nil)

// RecordClientAssertionReplay 依靠 (oauth_client_id, jti_hash) 唯一键原子占用断言标识，
// 过期时间用于后续清理；插入冲突即代表跨实例重放。
func (r *Repository) RecordClientAssertionReplay(ctx context.Context, oauthClientID string, jtiHash [32]byte, expiresAt, now time.Time) error {
	row := map[string]any{"oauth_client_id": oauthClientID, "jti_hash": jtiHash[:], "expires_at": expiresAt.UTC(), "created_at": now.UTC()}
	if err := r.database.WithContext(ctx).Table("oauth_client_assertion_replay").Create(row).Error; err != nil {
		if isDuplicateKey(err) {
			return application.ErrClientAssertionReplay
		}
		return err
	}
	return nil
}

// IsRegisteredPostLogoutRedirectURI accepts only an exact URI from the dedicated registration table.
func (r *Repository) IsRegisteredPostLogoutRedirectURI(ctx context.Context, clientID, redirectURI string, now time.Time) (bool, error) {
	client, err := r.findClient(ctx, r.database, "platform_oauth_client.client_id = ?", clientID)
	if err != nil {
		return false, mapError(err)
	}
	var count int64
	err = r.database.WithContext(ctx).Table("platform_oauth_post_logout_redirect_uri").Where("oauth_client_id = ? AND post_logout_redirect_uri = ?", client.ID, redirectURI).Count(&count).Error
	return count == 1, err
}

func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}

var _ application.ClientAssertionReplayRepository = (*Repository)(nil)

func (r *Repository) FindConsent(ctx context.Context, tenantID, userID, oauthClientID string, _ time.Time) ([]string, bool, error) {
	var row struct {
		Scope     string
		Status    string
		RevokedAt *time.Time
	}
	if err := r.database.WithContext(ctx).Table("iam_oidc_user_consent").Where("tenant_id = ? AND user_id = ? AND oauth_client_id = ?", tenantID, userID, oauthClientID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return parseScopes(row.Scope), row.Status == activeStatus && row.RevokedAt == nil, nil
}
func (r *Repository) GrantConsent(ctx context.Context, tenantID, userID, oauthClientID string, scopes []string, now time.Time) error {
	// 同一用户与客户端只有一条同意聚合；重复授权整体替换 scope 并递增版本，撤销后再次授权也复用该行。
	row := map[string]any{"tenant_id": tenantID, "user_id": userID, "oauth_client_id": oauthClientID, "scope": joinScopes(scopes), "granted_at": now.UTC(), "revoked_at": nil, "updated_at": now.UTC(), "status": activeStatus, "version": 1}
	return r.database.WithContext(ctx).Table("iam_oidc_user_consent").Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "user_id"}, {Name: "oauth_client_id"}}, DoUpdates: clause.Assignments(map[string]any{"scope": joinScopes(scopes), "granted_at": now.UTC(), "revoked_at": nil, "updated_at": now.UTC(), "status": activeStatus, "version": gorm.Expr("version + 1")})}).Create(row).Error
}
func (r *Repository) RevokeConsent(ctx context.Context, tenantID, userID, oauthClientID string, now time.Time) error {
	result := r.database.WithContext(ctx).Table("iam_oidc_user_consent").Where("tenant_id = ? AND user_id = ? AND oauth_client_id = ?", tenantID, userID, oauthClientID).Updates(map[string]any{"revoked_at": now.UTC(), "updated_at": now.UTC(), "status": "REVOKED", "version": gorm.Expr("version + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return application.ErrNotFound
	}
	return nil
}

var _ application.ConsentRepository = (*Repository)(nil)
var _ application.PostLogoutRedirectRepository = (*Repository)(nil)

type parRow struct {
	ID                                                                   string
	TenantID                                                             string
	OAuthClientID                                                        string `gorm:"column:oauth_client_id"`
	RequestURIHash                                                       []byte
	RedirectURI, Scope, State, Nonce, CodeChallenge, CodeChallengeMethod string
	RequestObjectHash                                                    []byte
	CreatedAt, ExpiresAt                                                 time.Time
	ConsumedAt                                                           *time.Time
	Status                                                               string
}

func (r *Repository) CreatePushedAuthorizationRequest(ctx context.Context, request application.PushedAuthorizationRequest) error {
	var objectHash []byte
	if request.RequestObjectHash != nil {
		objectHash = request.RequestObjectHash[:]
	}
	return r.database.WithContext(ctx).Table("oauth_pushed_authorization_request").Create(map[string]any{"id": request.ID, "tenant_id": request.TenantID, "oauth_client_id": request.OAuthClientID, "request_uri_hash": request.RequestURIHash[:], "redirect_uri": request.RedirectURI, "scope": joinScopes(request.Scopes), "state": stringPointer(request.State), "nonce": stringPointer(request.Nonce), "code_challenge": stringPointer(request.CodeChallenge), "code_challenge_method": stringPointer(request.CodeChallengeMethod), "request_object_hash": objectHash, "created_at": request.CreatedAt.UTC(), "expires_at": request.ExpiresAt.UTC(), "status": activeStatus}).Error
}
func (r *Repository) ConsumePushedAuthorizationRequest(ctx context.Context, tenantID, oauthClientID string, hash [32]byte, now time.Time) (application.PushedAuthorizationRequest, error) {
	// request_uri 的查找、租户/客户端绑定、过期判断和状态转换必须在同一行锁内完成，
	// 否则两个并发浏览器请求可能把同一 PAR 兑换成两个授权码流程。
	var row parRow
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("oauth_pushed_authorization_request").Where("tenant_id = ? AND oauth_client_id = ? AND request_uri_hash = ?", tenantID, oauthClientID, hash[:]).Take(&row).Error; err != nil {
			return err
		}
		if row.Status != activeStatus || row.ConsumedAt != nil || !row.ExpiresAt.After(now) {
			return application.ErrAuthorizationCodeUnavailable
		}
		result := tx.Table("oauth_pushed_authorization_request").Where("id = ? AND status = ? AND consumed_at IS NULL", row.ID, activeStatus).Updates(map[string]any{"status": "CONSUMED", "consumed_at": now.UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrAuthorizationCodeUnavailable
		}
		return nil
	})
	if err != nil {
		return application.PushedAuthorizationRequest{}, mapError(err)
	}
	var objectHash *[32]byte
	if len(row.RequestObjectHash) == 32 {
		var value [32]byte
		copy(value[:], row.RequestObjectHash)
		objectHash = &value
	}
	var uriHash [32]byte
	copy(uriHash[:], row.RequestURIHash)
	return application.PushedAuthorizationRequest{ID: row.ID, TenantID: row.TenantID, OAuthClientID: row.OAuthClientID, RequestURIHash: uriHash, RedirectURI: row.RedirectURI, Scopes: parseScopes(row.Scope), State: row.State, Nonce: row.Nonce, CodeChallenge: row.CodeChallenge, CodeChallengeMethod: row.CodeChallengeMethod, RequestObjectHash: objectHash, CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC()}, nil
}

var _ application.PushedAuthorizationRequestRepository = (*Repository)(nil)
