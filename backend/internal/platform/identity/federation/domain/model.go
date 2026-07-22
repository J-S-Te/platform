// Package domain defines the durable, tenant-scoped federation aggregates.
package domain

import "time"

const (
	// ProviderTypeOIDC identifies a standard OpenID Connect provider.
	ProviderTypeOIDC = "OIDC"
	// ProviderTypeDingTalkQR identifies a DingTalk third-party enterprise application
	// that completes sign-in through a QR-code authorization flow.
	ProviderTypeDingTalkQR = "DINGTALK_QR"

	ProviderStatusActive   = "ACTIVE"
	ProviderStatusDisabled = "DISABLED"

	BindingStatusActive  = "ACTIVE"
	BindingStatusUnbound = "UNBOUND"
)

// Provider identifies an externally operated identity provider configured for a tenant. Issuer is
// required only for OIDC providers. For DINGTALK_QR, the compatibility field ClientID contains the ISV application SuiteKey and
// ClientSecretCiphertext contains the encrypted SuiteSecret/client secret.
//
// ClientSecretCiphertext is an AES-GCM protected value owned by the application layer. It is
// intentionally never serialized by the management HTTP adapter or written to application logs.
type Provider struct {
	ID                     string
	TenantID               string
	Code                   string
	Type                   string
	Issuer                 string
	ClientID               string
	CallbackURI            string
	AuthorizationScopes    []string
	ClientSecretCiphertext []byte
	ClientSecretUpdatedAt  *time.Time
	DisplayName            string
	Status                 string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Version                uint64
}

// HasClientSecret reports whether the provider has a persisted encrypted client secret. It is
// safe to expose this boolean to management clients; it never reveals the secret itself.
func (provider Provider) HasClientSecret() bool { return len(provider.ClientSecretCiphertext) > 0 }

// Binding links one verified upstream subject to a local user. SubjectHash is the only storage
// representation of the upstream subject; the raw upstream subject must never be put in this
// aggregate, API responses, logs, or persistence models.
type Binding struct {
	ID          string
	TenantID    string
	ProviderID  string
	UserID      string
	SubjectHash [32]byte
	BoundAt     time.Time
	UnboundAt   *time.Time
	Status      string
	Version     uint64
}
