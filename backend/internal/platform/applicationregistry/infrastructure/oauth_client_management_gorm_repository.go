package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	mysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OAuthClientManagementRepository persists OAuth client registration and credential lifecycle
// writes. It never returns or selects credential secret hashes for management responses.
type OAuthClientManagementRepository struct {
	database *gorm.DB
}

// NewOAuthClientManagementRepository constructs OAuth client management persistence.
func NewOAuthClientManagementRepository(database *gorm.DB) (*OAuthClientManagementRepository, error) {
	if database == nil {
		return nil, errors.New("OAuth client management database must not be nil")
	}
	return &OAuthClientManagementRepository{database: database}, nil
}

type oauthClientManagementModel struct {
	ID                     string    `gorm:"column:id;primaryKey"`
	TenantID               string    `gorm:"column:tenant_id"`
	ApplicationID          string    `gorm:"column:application_id"`
	EnvironmentID          string    `gorm:"column:environment_id"`
	ClientID               string    `gorm:"column:client_id"`
	ClientName             string    `gorm:"column:client_name"`
	ClientType             string    `gorm:"column:client_type"`
	TokenAuthMethod        string    `gorm:"column:token_auth_method"`
	AccessTokenTTLSeconds  uint      `gorm:"column:access_token_ttl_seconds"`
	RefreshTokenTTLSeconds uint      `gorm:"column:refresh_token_ttl_seconds"`
	RequirePKCE            bool      `gorm:"column:require_pkce"`
	Status                 string    `gorm:"column:status"`
	Version                uint64    `gorm:"column:version"`
	CreatedAt              time.Time `gorm:"column:created_at"`
	CreatedBy              *string   `gorm:"column:created_by"`
	UpdatedAt              time.Time `gorm:"column:updated_at"`
	UpdatedBy              *string   `gorm:"column:updated_by"`
}

func (oauthClientManagementModel) TableName() string { return "platform_oauth_client" }

type oauthClientEnvironmentModel struct {
	ID            string `gorm:"column:id;primaryKey"`
	TenantID      string `gorm:"column:tenant_id"`
	ApplicationID string `gorm:"column:application_id"`
	Status        string `gorm:"column:status"`
}

func (oauthClientEnvironmentModel) TableName() string { return "platform_application_environment" }

type oauthClientApplicationModel struct {
	ID       string `gorm:"column:id;primaryKey"`
	TenantID string `gorm:"column:tenant_id"`
	Status   string `gorm:"column:status"`
}

func (oauthClientApplicationModel) TableName() string { return "platform_application" }

type oauthClientRedirectURIModel struct {
	OAuthClientID string    `gorm:"column:oauth_client_id;primaryKey"`
	RedirectURI   string    `gorm:"column:redirect_uri;primaryKey"`
	CreatedAt     time.Time `gorm:"column:created_at"`
}

func (oauthClientRedirectURIModel) TableName() string { return "platform_oauth_redirect_uri" }

// oauthClientPostLogoutRedirectURIModel 映射独立的 RP-initiated logout 回调地址集合。
type oauthClientPostLogoutRedirectURIModel struct {
	OAuthClientID         string    `gorm:"column:oauth_client_id;primaryKey"`
	PostLogoutRedirectURI string    `gorm:"column:post_logout_redirect_uri;primaryKey"`
	CreatedAt             time.Time `gorm:"column:created_at"`
}

func (oauthClientPostLogoutRedirectURIModel) TableName() string {
	return "platform_oauth_post_logout_redirect_uri"
}

// oauthClientJWKModel 只映射客户端公钥。私钥字段在 application 层已拒绝，绝不可写入
// oauth_client_jwk。
type oauthClientJWKModel struct {
	ID            string     `gorm:"column:id;primaryKey"`
	OAuthClientID string     `gorm:"column:oauth_client_id"`
	KeyID         string     `gorm:"column:key_id"`
	PublicJWK     []byte     `gorm:"column:public_jwk;type:json"`
	Algorithm     *string    `gorm:"column:algorithm"`
	ValidFrom     time.Time  `gorm:"column:valid_from"`
	ValidUntil    *time.Time `gorm:"column:valid_until"`
	RevokedAt     *time.Time `gorm:"column:revoked_at"`
	Status        string     `gorm:"column:status"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

func (oauthClientJWKModel) TableName() string { return "oauth_client_jwk" }

type oauthClientGrantTypeModel struct {
	OAuthClientID string `gorm:"column:oauth_client_id;primaryKey"`
	GrantType     string `gorm:"column:grant_type;primaryKey"`
}

func (oauthClientGrantTypeModel) TableName() string { return "platform_oauth_grant_type" }

type oauthClientScopeModel struct {
	OAuthClientID string `gorm:"column:oauth_client_id;primaryKey"`
	ScopeCode     string `gorm:"column:scope_code;primaryKey"`
}

func (oauthClientScopeModel) TableName() string { return "platform_oauth_client_scope" }

type oauthClientCredentialModel struct {
	ID             string     `gorm:"column:id;primaryKey"`
	OAuthClientID  string     `gorm:"column:oauth_client_id"`
	CredentialType string     `gorm:"column:credential_type"`
	SecretHash     []byte     `gorm:"column:secret_hash"`
	Fingerprint    string     `gorm:"column:fingerprint"`
	ValidFrom      time.Time  `gorm:"column:valid_from"`
	ValidUntil     *time.Time `gorm:"column:valid_until"`
	RevokedAt      *time.Time `gorm:"column:revoked_at"`
	Status         string     `gorm:"column:status"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

func (oauthClientCredentialModel) TableName() string { return "platform_oauth_client_credential" }

const oauthClientSecretCredentialType = "secret"

type oauthClientCredentialViewRow struct {
	ID          string     `gorm:"column:id"`
	Fingerprint string     `gorm:"column:fingerprint"`
	ValidFrom   time.Time  `gorm:"column:valid_from"`
	ValidUntil  *time.Time `gorm:"column:valid_until"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	Status      string     `gorm:"column:status"`
}

// CreateOAuthClient 在同一事务中写入客户端、授权模式、scope、回调地址及可选初始密钥哈希。
// 任一子表失败都会回滚整个聚合；仓储接口从类型上不接受明文 secret。
func (repository *OAuthClientManagementRepository) CreateOAuthClient(ctx context.Context, input application.OAuthClientCreateInput, oauthClientID string, secret *application.SecretWrite, now time.Time) (application.OAuthClientView, error) {
	var result application.OAuthClientView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		if err := verifyOAuthClientParent(transaction, input.TenantID, input.ApplicationID, input.EnvironmentID); err != nil {
			return err
		}

		client := oauthClientManagementModel{
			ID: oauthClientID, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID,
			ClientID: input.ClientID, ClientName: input.ClientName, ClientType: input.ClientType, TokenAuthMethod: input.TokenAuthMethod,
			AccessTokenTTLSeconds: input.AccessTokenTTLSeconds, RefreshTokenTTLSeconds: input.RefreshTokenTTLSeconds, RequirePKCE: input.RequirePKCE,
			Status: "ACTIVE", Version: 1, CreatedAt: now, CreatedBy: oauthClientStringPointer(input.OperatorID), UpdatedAt: now, UpdatedBy: oauthClientStringPointer(input.OperatorID),
		}
		if err := transaction.Create(&client).Error; err != nil {
			return mapOAuthClientManagementError(err)
		}
		if err := createOAuthClientCapabilities(transaction, client.ID, input.GrantTypes, input.Scopes, input.RedirectURIs, now); err != nil {
			return err
		}
		if secret != nil {
			if err := createOAuthClientSecret(transaction, client.ID, *secret, now); err != nil {
				return err
			}
		}
		view, err := oauthClientManagementView(transaction, input.TenantID, client.ID)
		if err != nil {
			return err
		}
		result = view
		return nil
	})
	return result, mapOAuthClientManagementError(err)
}

// ListOAuthClients returns safe aggregate projections for the tenant.
func (repository *OAuthClientManagementRepository) ListOAuthClients(ctx context.Context, tenantID string) ([]application.OAuthClientView, error) {
	var clients []oauthClientManagementModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC, id DESC").Find(&clients).Error; err != nil {
		return nil, mapOAuthClientManagementError(err)
	}

	result := make([]application.OAuthClientView, 0, len(clients))
	for _, client := range clients {
		view, err := oauthClientManagementView(repository.database.WithContext(ctx), tenantID, client.ID)
		if err != nil {
			return nil, mapOAuthClientManagementError(err)
		}
		result = append(result, view)
	}
	return result, nil
}

// GetOAuthClient returns one safe aggregate projection scoped to the tenant.
func (repository *OAuthClientManagementRepository) GetOAuthClient(ctx context.Context, tenantID, oauthClientID string) (application.OAuthClientView, error) {
	view, err := oauthClientManagementView(repository.database.WithContext(ctx), tenantID, oauthClientID)
	return view, mapOAuthClientManagementError(err)
}

// ReplaceOAuthClientScopes 先锁定聚合并校验版本，再整体替换 scope 子表并递增版本；
// 管理员基于旧页面提交时不会静默覆盖另一管理员刚完成的安全收紧。
func (repository *OAuthClientManagementRepository) ReplaceOAuthClientScopes(ctx context.Context, input application.OAuthClientScopesUpdateInput, now time.Time) (application.OAuthClientView, error) {
	var result application.OAuthClientView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Version != input.Version {
			return application.ErrManagementConflict
		}
		if err := transaction.Where("oauth_client_id = ?", client.ID).Delete(&oauthClientScopeModel{}).Error; err != nil {
			return err
		}
		if err := createOAuthClientScopes(transaction, client.ID, input.Scopes); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, input.Version, input.OperatorID, now); err != nil {
			return err
		}
		result, err = oauthClientManagementView(transaction, input.TenantID, client.ID)
		return err
	})
	return result, mapOAuthClientManagementError(err)
}

// ReplaceOAuthClientRedirectURIs 以客户端聚合版本保护整组回调地址替换，避免逐条更新留下
// 新旧集合混合的中间状态或并发管理操作相互覆盖。
func (repository *OAuthClientManagementRepository) ReplaceOAuthClientRedirectURIs(ctx context.Context, input application.OAuthClientRedirectURIsUpdateInput, now time.Time) (application.OAuthClientView, error) {
	var result application.OAuthClientView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Version != input.Version {
			return application.ErrManagementConflict
		}
		if err := transaction.Where("oauth_client_id = ?", client.ID).Delete(&oauthClientRedirectURIModel{}).Error; err != nil {
			return err
		}
		if err := createOAuthClientRedirectURIs(transaction, client.ID, input.RedirectURIs, now); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, input.Version, input.OperatorID, now); err != nil {
			return err
		}
		result, err = oauthClientManagementView(transaction, input.TenantID, client.ID)
		return err
	})
	return result, mapOAuthClientManagementError(err)
}

// GetOAuthClientPostLogoutRedirectURIs 在租户边界内读取独立的登出后回调地址集合，并返回
// 当前聚合版本。
func (repository *OAuthClientManagementRepository) GetOAuthClientPostLogoutRedirectURIs(ctx context.Context, tenantID, oauthClientID string) (application.OAuthClientPostLogoutRedirectURIsView, error) {
	view, err := oauthClientPostLogoutRedirectURIsView(repository.database.WithContext(ctx), tenantID, oauthClientID)
	return view, mapOAuthClientManagementError(err)
}

// ReplaceOAuthClientPostLogoutRedirectURIs 在一个事务中替换整个独立登出后回调地址集合，并递增
// OAuth 客户端版本。
func (repository *OAuthClientManagementRepository) ReplaceOAuthClientPostLogoutRedirectURIs(ctx context.Context, input application.OAuthClientPostLogoutRedirectURIsUpdateInput, now time.Time) (application.OAuthClientPostLogoutRedirectURIsView, error) {
	var result application.OAuthClientPostLogoutRedirectURIsView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Version != input.Version {
			return application.ErrManagementConflict
		}
		if err := transaction.Where("oauth_client_id = ?", client.ID).Delete(&oauthClientPostLogoutRedirectURIModel{}).Error; err != nil {
			return err
		}
		if err := createOAuthClientPostLogoutRedirectURIs(transaction, client.ID, input.PostLogoutRedirectURIs, now); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, input.Version, input.OperatorID, now); err != nil {
			return err
		}
		result, err = oauthClientPostLogoutRedirectURIsView(transaction, input.TenantID, client.ID)
		return err
	})
	return result, mapOAuthClientManagementError(err)
}

// GetOAuthClientJWKs 返回在 OAuth 客户端租户边界内登记的公钥集合。持久化模型与响应中均无
// 私钥字段。
func (repository *OAuthClientManagementRepository) GetOAuthClientJWKs(ctx context.Context, tenantID, oauthClientID string) (application.OAuthClientJWKsView, error) {
	view, err := oauthClientJWKsView(repository.database.WithContext(ctx), tenantID, oauthClientID)
	return view, mapOAuthClientManagementError(err)
}

// ReplaceOAuthClientJWKs 原子替换公钥集合并递增 OAuth 客户端版本。删除旧键可以允许轮换后
// 复用 key_id，避免被已撤销键的唯一约束残留阻断。
func (repository *OAuthClientManagementRepository) ReplaceOAuthClientJWKs(ctx context.Context, input application.OAuthClientJWKsUpdateInput, now time.Time) (application.OAuthClientJWKsView, error) {
	var result application.OAuthClientJWKsView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Version != input.Version {
			return application.ErrManagementConflict
		}
		if err := transaction.Where("oauth_client_id = ?", client.ID).Delete(&oauthClientJWKModel{}).Error; err != nil {
			return err
		}
		if err := createOAuthClientJWKs(transaction, client.ID, input.JWKs, now); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, input.Version, input.OperatorID, now); err != nil {
			return err
		}
		result, err = oauthClientJWKsView(transaction, input.TenantID, client.ID)
		return err
	})
	return result, mapOAuthClientManagementError(err)
}

// DisableOAuthClient disables the aggregate and revokes all currently active secret versions.
func (repository *OAuthClientManagementRepository) DisableOAuthClient(ctx context.Context, input application.OAuthClientDisableInput, now time.Time) (application.OAuthClientView, error) {
	var result application.OAuthClientView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Version != input.Version || client.Status != "ACTIVE" {
			return application.ErrManagementConflict
		}
		updated := transaction.Model(&oauthClientManagementModel{}).
			Where("id = ? AND tenant_id = ? AND version = ? AND status = ?", client.ID, input.TenantID, input.Version, "ACTIVE").
			Updates(map[string]any{"status": "DISABLED", "version": input.Version + 1, "updated_at": now, "updated_by": input.OperatorID})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return application.ErrManagementConflict
		}
		if err := transaction.Model(&oauthClientCredentialModel{}).
			Where("oauth_client_id = ? AND credential_type = ? AND status = ?", client.ID, oauthClientSecretCredentialType, "ACTIVE").
			Updates(map[string]any{"status": "REVOKED", "revoked_at": now}).Error; err != nil {
			return err
		}
		result, err = oauthClientManagementView(transaction, input.TenantID, client.ID)
		return err
	})
	return result, mapOAuthClientManagementError(err)
}

// CreateOAuthClientSecret adds a credential version to an active client_secret_basic client.
func (repository *OAuthClientManagementRepository) CreateOAuthClientSecret(ctx context.Context, input application.OAuthClientSecretCreateInput, secret application.SecretWrite, now time.Time) (application.OAuthClientCredentialView, error) {
	var result application.OAuthClientCredentialView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Status != "ACTIVE" || client.TokenAuthMethod != "client_secret_basic" {
			return application.ErrManagementConflict
		}
		if err := createOAuthClientSecret(transaction, client.ID, secret, now); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, client.Version, input.OperatorID, now); err != nil {
			return err
		}
		result = secretToOAuthClientCredentialView(secret, now)
		return nil
	})
	return result, mapOAuthClientManagementError(err)
}

// RotateOAuthClientSecret 在一个事务中先收紧所有旧活跃密钥的有效期，再插入新版本。
// overlap=0 表示立即撤销；非零窗口只会缩短既有截止时间，绝不会把原本更早过期的密钥延长。
func (repository *OAuthClientManagementRepository) RotateOAuthClientSecret(ctx context.Context, input application.OAuthClientSecretRotateInput, secret application.SecretWrite, now time.Time) (application.OAuthClientCredentialView, error) {
	var result application.OAuthClientCredentialView
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		if client.Status != "ACTIVE" || client.TokenAuthMethod != "client_secret_basic" {
			return application.ErrManagementConflict
		}

		credentials := transaction.Model(&oauthClientCredentialModel{}).Where("oauth_client_id = ? AND credential_type = ? AND status = ?", client.ID, oauthClientSecretCredentialType, "ACTIVE")
		if input.OverlapSeconds == 0 {
			if err := credentials.Updates(map[string]any{"status": "REVOKED", "revoked_at": now}).Error; err != nil {
				return err
			}
		} else {
			overlapUntil := now.Add(time.Duration(input.OverlapSeconds) * time.Second)
			if err := credentials.Updates(map[string]any{
				"valid_until": gorm.Expr("CASE WHEN valid_until IS NULL OR valid_until > ? THEN ? ELSE valid_until END", overlapUntil, overlapUntil),
			}).Error; err != nil {
				return err
			}
		}
		if err := createOAuthClientSecret(transaction, client.ID, secret, now); err != nil {
			return err
		}
		if err := touchOAuthClient(transaction, client.ID, client.Version, input.OperatorID, now); err != nil {
			return err
		}
		result = secretToOAuthClientCredentialView(secret, now)
		return nil
	})
	return result, mapOAuthClientManagementError(err)
}

// DisableOAuthClientSecret revokes one secret credential without ever reading its hash.
func (repository *OAuthClientManagementRepository) DisableOAuthClientSecret(ctx context.Context, input application.OAuthClientSecretDisableInput, now time.Time) error {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		client, err := lockOAuthClient(transaction, input.TenantID, input.OAuthClientID)
		if err != nil {
			return err
		}
		updated := transaction.Model(&oauthClientCredentialModel{}).
			Where("id = ? AND oauth_client_id = ? AND credential_type = ? AND status = ?", input.CredentialID, client.ID, oauthClientSecretCredentialType, "ACTIVE").
			Updates(map[string]any{"status": "REVOKED", "revoked_at": now})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return application.ErrManagementNotFound
		}
		return touchOAuthClient(transaction, client.ID, client.Version, input.OperatorID, now)
	})
	return mapOAuthClientManagementError(err)
}

func verifyOAuthClientParent(transaction *gorm.DB, tenantID, applicationID, environmentID string) error {
	// 创建客户端前同时锁定应用和环境父记录，并要求环境确实属于该租户与应用；
	// 不能仅依赖全局唯一 ID 或前端级联选择来保证多租户归属。
	var applicationRow oauthClientApplicationModel
	if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND tenant_id = ? AND status = ?", applicationID, tenantID, "ACTIVE").Take(&applicationRow).Error; err != nil {
		return mapOAuthClientManagementError(err)
	}
	var environment oauthClientEnvironmentModel
	if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND tenant_id = ? AND application_id = ? AND status = ?", environmentID, tenantID, applicationID, "ACTIVE").Take(&environment).Error; err != nil {
		return mapOAuthClientManagementError(err)
	}
	return nil
}

func lockOAuthClient(transaction *gorm.DB, tenantID, oauthClientID string) (oauthClientManagementModel, error) {
	var client oauthClientManagementModel
	err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ?", oauthClientID, tenantID).Take(&client).Error
	return client, mapOAuthClientManagementError(err)
}

func touchOAuthClient(transaction *gorm.DB, oauthClientID string, version uint64, operatorID string, now time.Time) error {
	updated := transaction.Model(&oauthClientManagementModel{}).Where("id = ? AND version = ?", oauthClientID, version).
		Updates(map[string]any{"version": version + 1, "updated_at": now, "updated_by": operatorID})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return application.ErrManagementConflict
	}
	return nil
}

func createOAuthClientCapabilities(transaction *gorm.DB, oauthClientID string, grantTypes, scopes, redirectURIs []string, now time.Time) error {
	if len(grantTypes) > 0 {
		values := make([]oauthClientGrantTypeModel, 0, len(grantTypes))
		for _, grantType := range grantTypes {
			values = append(values, oauthClientGrantTypeModel{OAuthClientID: oauthClientID, GrantType: grantType})
		}
		if err := transaction.Create(&values).Error; err != nil {
			return err
		}
	}
	if err := createOAuthClientScopes(transaction, oauthClientID, scopes); err != nil {
		return err
	}
	return createOAuthClientRedirectURIs(transaction, oauthClientID, redirectURIs, now)
}

func createOAuthClientScopes(transaction *gorm.DB, oauthClientID string, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	values := make([]oauthClientScopeModel, 0, len(scopes))
	for _, scope := range scopes {
		values = append(values, oauthClientScopeModel{OAuthClientID: oauthClientID, ScopeCode: scope})
	}
	return transaction.Create(&values).Error
}

func createOAuthClientRedirectURIs(transaction *gorm.DB, oauthClientID string, redirectURIs []string, now time.Time) error {
	if len(redirectURIs) == 0 {
		return nil
	}
	values := make([]oauthClientRedirectURIModel, 0, len(redirectURIs))
	for _, redirectURI := range redirectURIs {
		values = append(values, oauthClientRedirectURIModel{OAuthClientID: oauthClientID, RedirectURI: redirectURI, CreatedAt: now})
	}
	return transaction.Create(&values).Error
}

func createOAuthClientPostLogoutRedirectURIs(transaction *gorm.DB, oauthClientID string, redirectURIs []string, now time.Time) error {
	if len(redirectURIs) == 0 {
		return nil
	}
	values := make([]oauthClientPostLogoutRedirectURIModel, 0, len(redirectURIs))
	for _, redirectURI := range redirectURIs {
		values = append(values, oauthClientPostLogoutRedirectURIModel{OAuthClientID: oauthClientID, PostLogoutRedirectURI: redirectURI, CreatedAt: now})
	}
	return transaction.Create(&values).Error
}

func createOAuthClientJWKs(transaction *gorm.DB, oauthClientID string, keys []application.OAuthClientJWK, now time.Time) error {
	if len(keys) == 0 {
		return nil
	}
	values := make([]oauthClientJWKModel, 0, len(keys))
	for _, key := range keys {
		values = append(values, oauthClientJWKModel{
			ID: key.ID, OAuthClientID: oauthClientID, KeyID: key.KeyID, PublicJWK: append([]byte(nil), key.PublicJWK...),
			Algorithm: oauthClientStringPointer(key.Algorithm), ValidFrom: key.ValidFrom.UTC(), ValidUntil: oauthClientTimePointer(key.ValidUntil),
			Status: key.Status, CreatedAt: now,
		})
	}
	return transaction.Create(&values).Error
}

func createOAuthClientSecret(transaction *gorm.DB, oauthClientID string, secret application.SecretWrite, now time.Time) error {
	return transaction.Create(&oauthClientCredentialModel{
		ID: secret.CredentialID, OAuthClientID: oauthClientID, CredentialType: oauthClientSecretCredentialType, SecretHash: append([]byte(nil), secret.SecretHash...),
		Fingerprint: secret.Fingerprint, ValidFrom: now, ValidUntil: oauthClientTimePointer(secret.ValidUntil), Status: "ACTIVE", CreatedAt: now,
	}).Error
}

func oauthClientPostLogoutRedirectURIsView(database *gorm.DB, tenantID, oauthClientID string) (application.OAuthClientPostLogoutRedirectURIsView, error) {
	var client oauthClientManagementModel
	if err := database.Where("id = ? AND tenant_id = ?", oauthClientID, tenantID).Take(&client).Error; err != nil {
		return application.OAuthClientPostLogoutRedirectURIsView{}, mapOAuthClientManagementError(err)
	}
	var rows []oauthClientPostLogoutRedirectURIModel
	if err := database.Where("oauth_client_id = ?", client.ID).Order("post_logout_redirect_uri ASC").Find(&rows).Error; err != nil {
		return application.OAuthClientPostLogoutRedirectURIsView{}, err
	}
	view := application.OAuthClientPostLogoutRedirectURIsView{OAuthClientID: client.ID, Version: client.Version, PostLogoutRedirectURIs: make([]string, 0, len(rows))}
	for _, row := range rows {
		view.PostLogoutRedirectURIs = append(view.PostLogoutRedirectURIs, row.PostLogoutRedirectURI)
	}
	return view, nil
}

func oauthClientJWKsView(database *gorm.DB, tenantID, oauthClientID string) (application.OAuthClientJWKsView, error) {
	var client oauthClientManagementModel
	if err := database.Where("id = ? AND tenant_id = ?", oauthClientID, tenantID).Take(&client).Error; err != nil {
		return application.OAuthClientJWKsView{}, mapOAuthClientManagementError(err)
	}
	var rows []oauthClientJWKModel
	if err := database.Where("oauth_client_id = ?", client.ID).Order("key_id ASC").Find(&rows).Error; err != nil {
		return application.OAuthClientJWKsView{}, err
	}
	view := application.OAuthClientJWKsView{OAuthClientID: client.ID, Version: client.Version, JWKs: make([]application.OAuthClientJWK, 0, len(rows))}
	for _, row := range rows {
		view.JWKs = append(view.JWKs, application.OAuthClientJWK{
			ID: row.ID, KeyID: row.KeyID, PublicJWK: append([]byte(nil), row.PublicJWK...), Algorithm: stringValue(row.Algorithm),
			ValidFrom: row.ValidFrom.UTC(), ValidUntil: oauthClientTimePointer(row.ValidUntil), Status: row.Status,
		})
	}
	return view, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func oauthClientManagementView(database *gorm.DB, tenantID, oauthClientID string) (application.OAuthClientView, error) {
	var client oauthClientManagementModel
	if err := database.Where("id = ? AND tenant_id = ?", oauthClientID, tenantID).Take(&client).Error; err != nil {
		return application.OAuthClientView{}, mapOAuthClientManagementError(err)
	}

	var grantRows []oauthClientGrantTypeModel
	if err := database.Where("oauth_client_id = ?", client.ID).Order("grant_type ASC").Find(&grantRows).Error; err != nil {
		return application.OAuthClientView{}, err
	}
	var scopeRows []oauthClientScopeModel
	if err := database.Where("oauth_client_id = ?", client.ID).Order("scope_code ASC").Find(&scopeRows).Error; err != nil {
		return application.OAuthClientView{}, err
	}
	var redirectRows []oauthClientRedirectURIModel
	if err := database.Where("oauth_client_id = ?", client.ID).Order("redirect_uri ASC").Find(&redirectRows).Error; err != nil {
		return application.OAuthClientView{}, err
	}
	var credentialRows []oauthClientCredentialViewRow
	if err := database.Table("platform_oauth_client_credential").
		Select("id, fingerprint, valid_from, valid_until, revoked_at, status").
		Where("oauth_client_id = ? AND credential_type = ?", client.ID, oauthClientSecretCredentialType).
		Order("valid_from DESC, id DESC").Scan(&credentialRows).Error; err != nil {
		return application.OAuthClientView{}, err
	}

	view := application.OAuthClientView{
		ID: client.ID, TenantID: client.TenantID, ApplicationID: client.ApplicationID, EnvironmentID: client.EnvironmentID,
		ClientID: client.ClientID, ClientName: client.ClientName, ClientType: client.ClientType, TokenAuthMethod: client.TokenAuthMethod,
		AccessTokenTTLSeconds: client.AccessTokenTTLSeconds, RefreshTokenTTLSeconds: client.RefreshTokenTTLSeconds, RequirePKCE: client.RequirePKCE,
		Status: client.Status, Version: client.Version, GrantTypes: make([]string, 0, len(grantRows)), Scopes: make([]string, 0, len(scopeRows)),
		RedirectURIs: make([]string, 0, len(redirectRows)), Credentials: make([]application.OAuthClientCredentialView, 0, len(credentialRows)),
		CreatedAt: client.CreatedAt, UpdatedAt: client.UpdatedAt,
	}
	for _, row := range grantRows {
		view.GrantTypes = append(view.GrantTypes, row.GrantType)
	}
	for _, row := range scopeRows {
		view.Scopes = append(view.Scopes, row.ScopeCode)
	}
	for _, row := range redirectRows {
		view.RedirectURIs = append(view.RedirectURIs, row.RedirectURI)
	}
	for _, row := range credentialRows {
		view.Credentials = append(view.Credentials, application.OAuthClientCredentialView{
			ID: row.ID, Fingerprint: row.Fingerprint, ValidFrom: row.ValidFrom, ValidUntil: oauthClientTimePointer(row.ValidUntil), RevokedAt: oauthClientTimePointer(row.RevokedAt), Status: row.Status,
		})
	}
	return view, nil
}

func secretToOAuthClientCredentialView(secret application.SecretWrite, now time.Time) application.OAuthClientCredentialView {
	return application.OAuthClientCredentialView{ID: secret.CredentialID, Fingerprint: secret.Fingerprint, ValidFrom: now, ValidUntil: oauthClientTimePointer(secret.ValidUntil), Status: "ACTIVE"}
}

func oauthClientStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func oauthClientTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func mapOAuthClientManagementError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, application.ErrManagementNotFound) || errors.Is(err, application.ErrManagementConflict) || errors.Is(err, application.ErrManagementValidation) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrManagementNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return application.ErrManagementConflict
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return application.ErrManagementConflict
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		return application.ErrManagementConflict
	}
	return fmt.Errorf("OAuth client management persistence: %w", err)
}
