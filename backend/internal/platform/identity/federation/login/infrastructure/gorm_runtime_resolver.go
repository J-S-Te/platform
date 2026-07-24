package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"

	identitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	federationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/application"
	federationdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/domain"
	loginapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/application"
	logindomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
	"gorm.io/gorm"
)

const (
	federatedProviderTable = "iam_federated_identity_provider"
	federatedBindingTable  = "iam_federated_identity_binding"
	identityTenantTable    = "iam_tenant"
	identityUserTable      = "iam_user"
	identityAccountTable   = "iam_account"
)

// GORMRuntimeResolver resolves only active, tenant-scoped federation runtime data. It is kept
// separate from management repositories because an external login needs a single authorization
// decision spanning federation, tenant, user and account state.
type GORMRuntimeResolver struct {
	database  *gorm.DB
	protector federationapplication.SecretProtector
}

var (
	_ loginapplication.ProviderResolver = (*GORMRuntimeResolver)(nil)
	_ loginapplication.AccountResolver  = (*GORMRuntimeResolver)(nil)
)

// NewGORMRuntimeResolver constructs the database-backed resolver used by the external-login
// application service. The protector must be configured with the dedicated provider-secret key.
func NewGORMRuntimeResolver(database *gorm.DB, protector federationapplication.SecretProtector) (*GORMRuntimeResolver, error) {
	if database == nil {
		return nil, loginapplication.ErrProviderUnavailable
	}
	if protector == nil {
		return nil, loginapplication.ErrProviderUnavailable
	}
	return &GORMRuntimeResolver{database: database, protector: protector}, nil
}

// ResolveProvider returns an active OIDC provider available to the specified tenant. All lookup,
// configuration and decryption failures intentionally collapse to ErrProviderUnavailable so the
// protocol edge cannot enumerate tenants, providers or secret-protection state.
func (resolver *GORMRuntimeResolver) ResolveProvider(ctx context.Context, tenantID, providerCode string) (logindomain.Provider, error) {
	tenantID = strings.TrimSpace(tenantID)
	providerCode = strings.TrimSpace(providerCode)
	if tenantID == "" || providerCode == "" {
		return logindomain.Provider{}, loginapplication.ErrProviderUnavailable
	}

	var row runtimeProviderRow
	err := resolver.database.WithContext(ctx).
		Table(federatedProviderTable+" AS provider").
		Select(`provider.tenant_id, provider.provider_code, provider.provider_type, provider.issuer,
			provider.client_id, provider.callback_uri, provider.authorization_scopes,
			provider.client_secret_ciphertext, provider.status`).
		Joins("JOIN "+identityTenantTable+" AS tenant ON tenant.id = provider.tenant_id").
		Where(`provider.tenant_id = ? AND provider.provider_code = ? AND provider.provider_type = ?
			AND provider.status = ? AND tenant.status = ?`,
			tenantID,
			providerCode,
			federationdomain.ProviderTypeOIDC,
			federationdomain.ProviderStatusActive,
			identitydomain.StatusActive,
		).
		Take(&row).Error
	if err != nil || !validRuntimeProviderRow(row, tenantID, providerCode) {
		return logindomain.Provider{}, loginapplication.ErrProviderUnavailable
	}

	scopes, err := decodeRuntimeScopes(row.AuthorizationScopes)
	if err != nil {
		return logindomain.Provider{}, loginapplication.ErrProviderUnavailable
	}
	clientSecret, err := resolver.protector.Decrypt(ctx, append([]byte(nil), row.ClientSecretCiphertext...))
	if err != nil || strings.TrimSpace(string(clientSecret)) == "" {
		return logindomain.Provider{}, loginapplication.ErrProviderUnavailable
	}

	return logindomain.Provider{
		TenantID:                 row.TenantID,
		Code:                     row.ProviderCode,
		Issuer:                   row.Issuer,
		ClientID:                 row.ClientID,
		ClientSecret:             string(clientSecret),
		RedirectURI:              row.CallbackURI,
		Scopes:                   append([]string(nil), scopes...),
		ClientAuthenticationMode: logindomain.ClientAuthenticationSecretBasic,
	}, nil
}

// ResolveAccount resolves a verified upstream subject to an active local account. The raw subject
// never leaves this method: only its SHA-256 digest is provided to GORM and therefore to MySQL.
// Every unavailable, inactive or inconsistent record is returned as ErrAccountNotBound to avoid
// revealing whether a binding, user, account, tenant or provider exists.
func (resolver *GORMRuntimeResolver) ResolveAccount(ctx context.Context, tenantID, providerCode, subject string) (logindomain.LocalAccount, error) {
	tenantID = strings.TrimSpace(tenantID)
	providerCode = strings.TrimSpace(providerCode)
	subject = strings.TrimSpace(subject)
	if tenantID == "" || providerCode == "" || subject == "" {
		return logindomain.LocalAccount{}, loginapplication.ErrAccountNotBound
	}

	subjectHash := sha256.Sum256([]byte(subject))
	var row runtimeAccountRow
	err := resolver.database.WithContext(ctx).
		Table(federatedBindingTable+" AS binding").
		Select(`binding.tenant_id AS tenant_id, provider.id AS provider_id, binding.id AS binding_id,
			binding.user_id AS user_id, account.id AS account_id`).
		Joins("JOIN "+federatedProviderTable+" AS provider ON provider.id = binding.provider_id AND provider.tenant_id = binding.tenant_id AND provider.provider_type = ? AND provider.status = ?", federationdomain.ProviderTypeOIDC, federationdomain.ProviderStatusActive).
		Joins("JOIN "+identityTenantTable+" AS tenant ON tenant.id = binding.tenant_id AND tenant.status = ?", identitydomain.StatusActive).
		Joins("JOIN "+identityUserTable+" AS user ON user.id = binding.user_id AND user.tenant_id = binding.tenant_id AND user.status = ?", identitydomain.StatusActive).
		Joins("JOIN "+identityAccountTable+" AS account ON account.user_id = binding.user_id AND account.tenant_id = binding.tenant_id AND account.status = ?", identitydomain.StatusActive).
		Where(`binding.tenant_id = ? AND provider.provider_code = ? AND binding.subject_hash = ?
			AND binding.status = ?`, tenantID, providerCode, subjectHash[:], federationdomain.BindingStatusActive).
		Order("account.created_at ASC, account.id ASC").
		Take(&row).Error
	if err != nil || !validRuntimeAccountRow(row, tenantID) {
		return logindomain.LocalAccount{}, loginapplication.ErrAccountNotBound
	}

	return logindomain.LocalAccount{
		TenantID:   row.TenantID,
		ProviderID: row.ProviderID,
		BindingID:  row.BindingID,
		UserID:     row.UserID,
		AccountID:  row.AccountID,
	}, nil
}

type runtimeProviderRow struct {
	TenantID               string `gorm:"column:tenant_id"`
	ProviderCode           string `gorm:"column:provider_code"`
	ProviderType           string `gorm:"column:provider_type"`
	Issuer                 string `gorm:"column:issuer"`
	ClientID               string `gorm:"column:client_id"`
	CallbackURI            string `gorm:"column:callback_uri"`
	AuthorizationScopes    []byte `gorm:"column:authorization_scopes"`
	ClientSecretCiphertext []byte `gorm:"column:client_secret_ciphertext"`
	Status                 string `gorm:"column:status"`
}

type runtimeAccountRow struct {
	TenantID   string `gorm:"column:tenant_id"`
	ProviderID string `gorm:"column:provider_id"`
	BindingID  string `gorm:"column:binding_id"`
	UserID     string `gorm:"column:user_id"`
	AccountID  string `gorm:"column:account_id"`
}

func validRuntimeProviderRow(row runtimeProviderRow, tenantID, providerCode string) bool {
	return row.TenantID == tenantID && row.ProviderCode == providerCode && row.ProviderType == federationdomain.ProviderTypeOIDC &&
		row.Status == federationdomain.ProviderStatusActive && strings.TrimSpace(row.Issuer) != "" &&
		strings.TrimSpace(row.ClientID) != "" && strings.TrimSpace(row.CallbackURI) != "" &&
		len(row.ClientSecretCiphertext) > 0
}

func decodeRuntimeScopes(encoded []byte) ([]string, error) {
	var scopes []string
	if err := json.Unmarshal(encoded, &scopes); err != nil || len(scopes) == 0 {
		return nil, loginapplication.ErrProviderUnavailable
	}
	for index := range scopes {
		scopes[index] = strings.TrimSpace(scopes[index])
		if scopes[index] == "" {
			return nil, loginapplication.ErrProviderUnavailable
		}
	}
	return scopes, nil
}

func validRuntimeAccountRow(row runtimeAccountRow, tenantID string) bool {
	return row.TenantID == tenantID && strings.TrimSpace(row.ProviderID) != "" && strings.TrimSpace(row.BindingID) != "" &&
		strings.TrimSpace(row.UserID) != "" && strings.TrimSpace(row.AccountID) != ""
}
