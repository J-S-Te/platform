// Package infrastructure loads OAuth application client registrations through GORM.
package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/domain"
	"gorm.io/gorm"
)

type Repository struct{ database *gorm.DB }

func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("application registry database must not be nil")
	}
	return &Repository{database: database}, nil
}

type clientModel struct {
	ID                    string
	TenantID              string
	ApplicationID         string
	EnvironmentID         string
	ClientID              string
	TokenAuthMethod       string
	AccessTokenTTLSeconds uint
	Status                string
	ApplicationCode       string `gorm:"column:application_code"`
	EnvironmentCode       string `gorm:"column:environment_code"`
}

type credentialModel struct {
	SecretHash []byte
	ValidUntil *time.Time
}

type stringRow struct{ Value string }

func (r *Repository) FindForClientCredentials(ctx context.Context, clientID string, now time.Time) (domain.OAuthClient, []domain.ClientCredential, error) {
	client, err := r.findClient(ctx, "platform_oauth_client.client_id = ?", clientID)
	if err != nil {
		return domain.OAuthClient{}, nil, mapError(err)
	}

	var credentials []credentialModel
	if err := r.database.WithContext(ctx).
		Table("platform_oauth_client_credential").
		Select("secret_hash, valid_until").
		Where("oauth_client_id = ? AND credential_type = ? AND status = ? AND revoked_at IS NULL AND valid_from <= ? AND (valid_until IS NULL OR valid_until > ?)", client.ID, "secret", "ACTIVE", now, now).
		Find(&credentials).Error; err != nil {
		return domain.OAuthClient{}, nil, err
	}

	return r.withCapabilities(ctx, client, credentials)
}

func (r *Repository) FindActiveByID(ctx context.Context, clientID string, _ time.Time) (domain.OAuthClient, error) {
	client, err := r.findClient(ctx, "platform_oauth_client.id = ?", clientID)
	if err != nil {
		return domain.OAuthClient{}, mapError(err)
	}
	result, _, err := r.withCapabilities(ctx, client, nil)
	return result, err
}

func (r *Repository) findClient(ctx context.Context, condition string, value string) (clientModel, error) {
	var client clientModel
	err := r.database.WithContext(ctx).
		Table("platform_oauth_client").
		Select("platform_oauth_client.id, platform_oauth_client.tenant_id, platform_oauth_client.application_id, platform_oauth_client.environment_id, platform_oauth_client.client_id, platform_oauth_client.token_auth_method, platform_oauth_client.access_token_ttl_seconds, platform_oauth_client.status, platform_application.code AS application_code, platform_application_environment.environment AS environment_code").
		Joins("JOIN platform_application ON platform_application.id = platform_oauth_client.application_id AND platform_application.status = ?", "ACTIVE").
		Joins("JOIN platform_application_environment ON platform_application_environment.id = platform_oauth_client.environment_id AND platform_application_environment.status = ?", "ACTIVE").
		Where(condition+" AND platform_oauth_client.status = ?", value, "ACTIVE").
		Take(&client).Error
	return client, err
}

func (r *Repository) withCapabilities(ctx context.Context, client clientModel, credentials []credentialModel) (domain.OAuthClient, []domain.ClientCredential, error) {
	var grants []stringRow
	if err := r.database.WithContext(ctx).Table("platform_oauth_grant_type").Select("grant_type AS value").Where("oauth_client_id = ?", client.ID).Find(&grants).Error; err != nil {
		return domain.OAuthClient{}, nil, err
	}
	var scopeRows []stringRow
	if err := r.database.WithContext(ctx).Table("platform_oauth_client_scope").Select("scope_code AS value").Where("oauth_client_id = ?", client.ID).Find(&scopeRows).Error; err != nil {
		return domain.OAuthClient{}, nil, err
	}

	result := domain.OAuthClient{
		ID: client.ID, TenantID: client.TenantID, ApplicationID: client.ApplicationID, ApplicationCode: client.ApplicationCode,
		EnvironmentID: client.EnvironmentID, EnvironmentCode: client.EnvironmentCode, ClientID: client.ClientID,
		TokenAuthMethod: client.TokenAuthMethod, AccessTokenTTLSeconds: client.AccessTokenTTLSeconds,
		GrantTypes: make(map[string]struct{}, len(grants)), Scopes: make(map[string]struct{}, len(scopeRows)),
	}
	for _, grant := range grants {
		result.GrantTypes[grant.Value] = struct{}{}
	}
	for _, scope := range scopeRows {
		result.Scopes[scope.Value] = struct{}{}
	}

	resultCredentials := make([]domain.ClientCredential, 0, len(credentials))
	for _, credential := range credentials {
		resultCredentials = append(resultCredentials, domain.ClientCredential{SecretHash: append([]byte(nil), credential.SecretHash...), ValidUntil: credential.ValidUntil})
	}
	return result, resultCredentials, nil
}

func mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrUnauthenticated
	}
	return err
}
