package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"time"

	identitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	federationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/domain"
	federationdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/domain"
	"gorm.io/gorm"
)

const (
	dingTalkProviderTable = "iam_federated_identity_provider"
	dingTalkBindingTable  = "iam_federated_identity_binding"
	dingTalkTenantTable   = "iam_tenant"
	dingTalkUserTable     = "iam_user"
	dingTalkAccountTable  = "iam_account"
)

// GORMRuntimeResolver makes a single active-state authorization decision across the federation,
// tenant, user and account tables. It does not expose whether a missing record was disabled,
// unbound or unknown.
type GORMRuntimeResolver struct {
	database  *gorm.DB
	protector federationapplication.SecretProtector
}

var (
	_ application.ProviderResolver = (*GORMRuntimeResolver)(nil)
	_ application.AccountResolver  = (*GORMRuntimeResolver)(nil)
)

// NewGORMRuntimeResolver creates the resolver with the provider-secret protector used by the
// existing federation management module.
func NewGORMRuntimeResolver(database *gorm.DB, protector federationapplication.SecretProtector) (*GORMRuntimeResolver, error) {
	if database == nil || protector == nil {
		return nil, application.ErrProviderUnavailable
	}
	return &GORMRuntimeResolver{database: database, protector: protector}, nil
}

// ResolveProvider returns an active DINGTALK_QR provider. The persisted ClientID is the ISV
// SuiteKey; the SuiteSecret/client secret is decrypted only in memory for the immediate token request.
func (resolver *GORMRuntimeResolver) ResolveProvider(ctx context.Context, tenantID, providerCode string) (domain.Provider, error) {
	tenantID, providerCode = strings.TrimSpace(tenantID), strings.TrimSpace(providerCode)
	if tenantID == "" || providerCode == "" {
		return domain.Provider{}, application.ErrProviderUnavailable
	}
	var row dingTalkRuntimeProviderRow
	err := resolver.database.WithContext(ctx).Table(dingTalkProviderTable+" AS provider").
		Select(`provider.id, provider.tenant_id, provider.provider_code, provider.provider_type, provider.client_id,
			provider.callback_uri, provider.authorization_scopes, provider.client_secret_ciphertext, provider.status`).
		Joins("JOIN "+dingTalkTenantTable+" AS tenant ON tenant.id = provider.tenant_id").
		Where(`provider.tenant_id = ? AND provider.provider_code = ? AND provider.provider_type = ?
			AND provider.status = ? AND tenant.status = ?`, tenantID, providerCode, federationdomain.ProviderTypeDingTalkQR,
			federationdomain.ProviderStatusActive, identitydomain.StatusActive).Take(&row).Error
	if err != nil || !validDingTalkProviderRow(row, tenantID, providerCode) {
		return domain.Provider{}, application.ErrProviderUnavailable
	}
	scopes, err := decodeDingTalkScopes(row.AuthorizationScopes)
	if err != nil {
		return domain.Provider{}, application.ErrProviderUnavailable
	}
	secret, err := resolver.protector.Decrypt(ctx, append([]byte(nil), row.ClientSecretCiphertext...))
	if err != nil || strings.TrimSpace(string(secret)) == "" {
		return domain.Provider{}, application.ErrProviderUnavailable
	}
	return domain.Provider{ID: row.ID, TenantID: row.TenantID, Code: row.ProviderCode, AppKey: row.ClientID,
		AppSecret: string(secret), RedirectURI: row.CallbackURI, Scopes: scopes}, nil
}

// ResolveAccount hashes unionID before issuing the only database query. The raw DingTalk identity
// never reaches SQL logs, storage, audit event metadata or application logs.
func (resolver *GORMRuntimeResolver) ResolveAccount(ctx context.Context, tenantID, providerCode, unionID string) (domain.LocalAccount, error) {
	tenantID, providerCode, unionID = strings.TrimSpace(tenantID), strings.TrimSpace(providerCode), strings.TrimSpace(unionID)
	if tenantID == "" || providerCode == "" || unionID == "" {
		return domain.LocalAccount{}, application.ErrAccountNotBound
	}
	subjectHash := sha256.Sum256([]byte(unionID))
	var row dingTalkRuntimeAccountRow
	err := resolver.database.WithContext(ctx).Table(dingTalkBindingTable+" AS binding").
		Select(`binding.tenant_id AS tenant_id, provider.id AS provider_id, binding.id AS binding_id,
			binding.user_id AS user_id, account.id AS account_id`).
		Joins("JOIN "+dingTalkProviderTable+" AS provider ON provider.id = binding.provider_id AND provider.tenant_id = binding.tenant_id AND provider.provider_type = ? AND provider.status = ?", federationdomain.ProviderTypeDingTalkQR, federationdomain.ProviderStatusActive).
		Joins("JOIN "+dingTalkTenantTable+" AS tenant ON tenant.id = binding.tenant_id AND tenant.status = ?", identitydomain.StatusActive).
		Joins("JOIN "+dingTalkUserTable+" AS user ON user.id = binding.user_id AND user.tenant_id = binding.tenant_id AND user.status = ?", identitydomain.StatusActive).
		Joins("JOIN "+dingTalkAccountTable+" AS account ON account.user_id = binding.user_id AND account.tenant_id = binding.tenant_id AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", identitydomain.StatusActive, time.Now().UTC()).
		Where(`binding.tenant_id = ? AND provider.provider_code = ? AND binding.subject_hash = ? AND binding.status = ?`, tenantID, providerCode, subjectHash[:], federationdomain.BindingStatusActive).
		Order("account.created_at ASC, account.id ASC").Take(&row).Error
	if err != nil || !validDingTalkAccountRow(row, tenantID) {
		return domain.LocalAccount{}, application.ErrAccountNotBound
	}
	return domain.LocalAccount{TenantID: row.TenantID, ProviderID: row.ProviderID, BindingID: row.BindingID, UserID: row.UserID, AccountID: row.AccountID}, nil
}

type dingTalkRuntimeProviderRow struct {
	ID                     string `gorm:"column:id"`
	TenantID               string `gorm:"column:tenant_id"`
	ProviderCode           string `gorm:"column:provider_code"`
	ProviderType           string `gorm:"column:provider_type"`
	ClientID               string `gorm:"column:client_id"`
	CallbackURI            string `gorm:"column:callback_uri"`
	AuthorizationScopes    []byte `gorm:"column:authorization_scopes"`
	ClientSecretCiphertext []byte `gorm:"column:client_secret_ciphertext"`
	Status                 string `gorm:"column:status"`
}

type dingTalkRuntimeAccountRow struct {
	TenantID   string `gorm:"column:tenant_id"`
	ProviderID string `gorm:"column:provider_id"`
	BindingID  string `gorm:"column:binding_id"`
	UserID     string `gorm:"column:user_id"`
	AccountID  string `gorm:"column:account_id"`
}

func validDingTalkProviderRow(row dingTalkRuntimeProviderRow, tenantID, providerCode string) bool {
	return row.TenantID == tenantID && row.ProviderCode == providerCode && row.ProviderType == federationdomain.ProviderTypeDingTalkQR &&
		row.Status == federationdomain.ProviderStatusActive && strings.TrimSpace(row.ID) != "" && strings.TrimSpace(row.ClientID) != "" &&
		strings.TrimSpace(row.CallbackURI) != "" && len(row.ClientSecretCiphertext) > 0
}

func decodeDingTalkScopes(encoded []byte) ([]string, error) {
	var scopes []string
	if err := json.Unmarshal(encoded, &scopes); err != nil || len(scopes) == 0 {
		return nil, application.ErrProviderUnavailable
	}
	for index := range scopes {
		scopes[index] = strings.TrimSpace(scopes[index])
		if scopes[index] == "" {
			return nil, application.ErrProviderUnavailable
		}
	}
	return scopes, nil
}

func validDingTalkAccountRow(row dingTalkRuntimeAccountRow, tenantID string) bool {
	return row.TenantID == tenantID && strings.TrimSpace(row.ProviderID) != "" && strings.TrimSpace(row.BindingID) != "" &&
		strings.TrimSpace(row.UserID) != "" && strings.TrimSpace(row.AccountID) != ""
}
