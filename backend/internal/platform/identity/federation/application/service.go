// Package application coordinates tenant-scoped external-identity provider configuration and
// bindings. Remote OAuth/OIDC discovery, token exchange, and assertion verification are kept at
// the protocol edge; this service owns durable configuration and local account association only.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/domain"
	logindomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

var (
	ErrInvalidRequest              = errors.New("invalid federation request")
	ErrNotFound                    = errors.New("federated identity not found")
	ErrConflict                    = errors.New("federated identity conflict")
	ErrVersionConflict             = errors.New("federated identity version conflict")
	ErrSecretProtectionUnavailable = errors.New("federation client-secret protection is unavailable")
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
	maxScopes       = 32
)

// Clock supplies UTC instants for durable state transitions.
type Clock interface{ Now() time.Time }

// SystemClock provides the production UTC clock for federation state transitions.
type SystemClock struct{}

// Now returns the current instant in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator creates sortable, durable identifiers.
type IDGenerator interface {
	New(time.Time) (string, error)
}

// SecretProtector encrypts an external provider client secret before it reaches persistence.
// Implementations must reject unavailable encryption rather than allowing a plaintext fallback.
type SecretProtector interface {
	Encrypt(context.Context, []byte) ([]byte, error)
	Decrypt(context.Context, []byte) ([]byte, error)
}

// Repository owns all table access. Its methods are deliberately tenant-scoped so the
// application service cannot accidentally perform a cross-tenant operation.
type Repository interface {
	CreateProvider(context.Context, domain.Provider) error
	ListProviders(context.Context, string, PageRequest) (PageResult[domain.Provider], error)
	FindProviderByID(context.Context, string, string) (domain.Provider, error)
	FindProviderByCode(context.Context, string, string) (domain.Provider, error)
	UpdateProvider(context.Context, ProviderPersistenceUpdate, time.Time) (domain.Provider, error)

	UserExists(context.Context, string, string) (bool, error)
	Bind(context.Context, domain.Binding) (domain.Binding, error)
	ListBindings(context.Context, string, string) ([]domain.Binding, error)
	FindBindingByID(context.Context, string, string) (domain.Binding, error)
	UnbindBinding(context.Context, string, string, string, uint64, time.Time) (domain.Binding, error)
	ResolveActiveBinding(context.Context, string, string, [32]byte) (domain.Binding, error)
}

// PageRequest bounds provider-management queries.
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
	Status   string
}

// PageResult is a tenant-scoped management list response.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// ProviderUpdate contains management-request fields. A nil runtime setting means leave the
// persisted value unchanged. ClientSecret is intentionally converted to ciphertext before the
// repository boundary and must never be persisted directly.
type ProviderUpdate struct {
	TenantID            string
	OperatorID          string
	ProviderID          string
	DisplayName         string
	Status              string
	ClientID            *string
	CallbackURI         *string
	AuthorizationScopes *[]string
	ClientSecret        *string
	Version             uint64
}

// ProviderPersistenceUpdate contains only safe-to-persist provider fields. Its secret field is
// already encrypted and no plaintext client credential crosses into infrastructure.
type ProviderPersistenceUpdate struct {
	TenantID               string
	ProviderID             string
	DisplayName            string
	Status                 string
	ClientID               *string
	CallbackURI            *string
	AuthorizationScopes    *[]string
	ClientSecretCiphertext *[]byte
	ClientSecretUpdatedAt  *time.Time
	Version                uint64
}

// RuntimeProvider is returned only to the trusted external-login protocol adapter. It contains a
// decrypted client secret and must never be returned from a management endpoint or logged.
type RuntimeProvider struct {
	Provider     domain.Provider
	ClientSecret string
}

type Service struct {
	repository Repository
	ids        IDGenerator
	clock      Clock
	protector  SecretProtector
}

// NewService constructs the local federation application service. The optional form preserves
// compatibility while runtime configuration writes fail closed until Bootstrap injects the
// dedicated client-secret protector. At most one protector may be supplied.
func NewService(repository Repository, ids IDGenerator, clock Clock, protectors ...SecretProtector) (*Service, error) {
	if repository == nil || ids == nil || clock == nil || len(protectors) > 1 {
		return nil, errors.New("federation dependencies must not be nil")
	}
	service := &Service{repository: repository, ids: ids, clock: clock}
	if len(protectors) == 1 {
		if protectors[0] == nil {
			return nil, errors.New("federation client-secret protector must not be nil")
		}
		service.protector = protectors[0]
	}
	return service, nil
}

// CreateProviderInput is the full identity-provider configuration needed by the external-login
// edge. For DINGTALK_QR, ClientID and ClientSecret respectively carry the ISV application SuiteKey and
// SuiteSecret/client secret. ClientSecret is accepted only for immediate encryption and is never returned in
// Provider.
type CreateProviderInput struct {
	TenantID            string
	OperatorID          string
	Code                string
	Type                string
	Issuer              string
	ClientID            string
	ClientSecret        string
	CallbackURI         string
	AuthorizationScopes []string
	DisplayName         string
}

// CreateProvider registers a tenant-owned identity provider. The client secret or DingTalk SuiteSecret is
// encrypted before persistence. The service deliberately fails closed when its dedicated
// protector is unavailable.
func (service *Service) CreateProvider(ctx context.Context, input CreateProviderInput) (domain.Provider, error) {
	input = normalizeCreateProvider(input)
	if !validCreateProvider(input) {
		return domain.Provider{}, ErrInvalidRequest
	}
	if service.protector == nil {
		return domain.Provider{}, ErrSecretProtectionUnavailable
	}

	ciphertext, err := service.protector.Encrypt(ctx, []byte(input.ClientSecret))
	if err != nil {
		return domain.Provider{}, fmt.Errorf("encrypt federated provider client secret: %w", err)
	}
	if len(ciphertext) == 0 {
		return domain.Provider{}, errors.New("encrypt federated provider client secret: empty ciphertext")
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Provider{}, fmt.Errorf("generate federation provider ID: %w", err)
	}
	secretUpdatedAt := now
	provider := domain.Provider{
		ID: id, TenantID: input.TenantID, Code: input.Code, Type: input.Type, Issuer: input.Issuer,
		ClientID: input.ClientID, CallbackURI: input.CallbackURI, AuthorizationScopes: cloneStrings(input.AuthorizationScopes),
		ClientSecretCiphertext: cloneBytes(ciphertext), ClientSecretUpdatedAt: &secretUpdatedAt,
		DisplayName: input.DisplayName, Status: domain.ProviderStatusActive,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := service.repository.CreateProvider(ctx, provider); err != nil {
		return domain.Provider{}, err
	}
	return provider, nil
}

// ListProviders returns management-safe provider configuration for the current tenant.
func (service *Service) ListProviders(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Provider], error) {
	tenantID = strings.TrimSpace(tenantID)
	query = normalizePageRequest(query)
	if tenantID == "" || query.PageSize > maxPageSize || !validProviderStatusFilter(query.Status) {
		return PageResult[domain.Provider]{}, ErrInvalidRequest
	}
	return service.repository.ListProviders(ctx, tenantID, query)
}

// GetProvider reads a provider regardless of status so management can inspect disabled records.
func (service *Service) GetProvider(ctx context.Context, tenantID, providerID string) (domain.Provider, error) {
	tenantID = strings.TrimSpace(tenantID)
	providerID = strings.TrimSpace(providerID)
	if tenantID == "" || providerID == "" {
		return domain.Provider{}, ErrInvalidRequest
	}
	return service.repository.FindProviderByID(ctx, tenantID, providerID)
}

// UpdateProvider changes management-safe fields and optionally rotates runtime configuration.
// The plaintext client secret is encrypted here and never reaches the repository boundary.
func (service *Service) UpdateProvider(ctx context.Context, input ProviderUpdate) (domain.Provider, error) {
	input = normalizeProviderUpdate(input)
	if input.TenantID == "" || input.OperatorID == "" || input.ProviderID == "" || !validDisplayName(input.DisplayName) ||
		!validProviderStatus(input.Status) || input.Version == 0 {
		return domain.Provider{}, ErrInvalidRequest
	}

	existing, err := service.repository.FindProviderByID(ctx, input.TenantID, input.ProviderID)
	if err != nil {
		return domain.Provider{}, err
	}
	if !validOptionalRuntimeConfiguration(existing.Type, input) {
		return domain.Provider{}, ErrInvalidRequest
	}

	persistence := ProviderPersistenceUpdate{
		TenantID: input.TenantID, ProviderID: input.ProviderID, DisplayName: input.DisplayName,
		Status: input.Status, ClientID: input.ClientID, CallbackURI: input.CallbackURI,
		AuthorizationScopes: input.AuthorizationScopes, Version: input.Version,
	}
	if input.ClientSecret != nil {
		if service.protector == nil {
			return domain.Provider{}, ErrSecretProtectionUnavailable
		}
		ciphertext, err := service.protector.Encrypt(ctx, []byte(*input.ClientSecret))
		if err != nil {
			return domain.Provider{}, fmt.Errorf("encrypt federated provider client secret: %w", err)
		}
		if len(ciphertext) == 0 {
			return domain.Provider{}, errors.New("encrypt federated provider client secret: empty ciphertext")
		}
		ciphertext = cloneBytes(ciphertext)
		now := service.clock.Now().UTC()
		persistence.ClientSecretCiphertext = &ciphertext
		persistence.ClientSecretUpdatedAt = &now
	}
	return service.repository.UpdateProvider(ctx, persistence, service.clock.Now().UTC())
}

// ResolveActiveRuntimeProvider returns decrypted runtime credentials only to a trusted external
// identity protocol adapter. Management code must use GetProvider/ListProviders instead.
func (service *Service) ResolveActiveRuntimeProvider(ctx context.Context, tenantID, providerCode string) (RuntimeProvider, error) {
	tenantID = strings.TrimSpace(tenantID)
	providerCode = strings.TrimSpace(providerCode)
	if tenantID == "" || !validCode(providerCode, 64) {
		return RuntimeProvider{}, ErrInvalidRequest
	}
	if service.protector == nil {
		return RuntimeProvider{}, ErrSecretProtectionUnavailable
	}
	provider, err := service.repository.FindProviderByCode(ctx, tenantID, providerCode)
	if err != nil {
		return RuntimeProvider{}, err
	}
	if provider.Status != domain.ProviderStatusActive || !validPersistedRuntimeConfiguration(provider) {
		return RuntimeProvider{}, ErrNotFound
	}
	secret, err := service.protector.Decrypt(ctx, provider.ClientSecretCiphertext)
	if err != nil {
		return RuntimeProvider{}, fmt.Errorf("decrypt federated provider client secret: %w", err)
	}
	if !validClientSecret(string(secret)) {
		return RuntimeProvider{}, errors.New("decrypt federated provider client secret: invalid plaintext")
	}
	return RuntimeProvider{Provider: provider, ClientSecret: string(secret)}, nil
}

// ResolveProvider implements the trusted OIDC external-login ProviderResolver contract. It keeps
// the decrypted value local to the protocol layer and never lets management endpoints access it.
// DINGTALK_QR is intentionally excluded because its QR authorization exchange is not OIDC.
func (service *Service) ResolveProvider(ctx context.Context, tenantID, providerCode string) (logindomain.Provider, error) {
	runtime, err := service.ResolveActiveRuntimeProvider(ctx, tenantID, providerCode)
	if err != nil {
		return logindomain.Provider{}, err
	}
	if runtime.Provider.Type != domain.ProviderTypeOIDC {
		return logindomain.Provider{}, ErrNotFound
	}
	return logindomain.Provider{
		TenantID: runtime.Provider.TenantID, Code: runtime.Provider.Code, Issuer: runtime.Provider.Issuer,
		ClientID: runtime.Provider.ClientID, ClientSecret: runtime.ClientSecret, RedirectURI: runtime.Provider.CallbackURI,
		Scopes: cloneStrings(runtime.Provider.AuthorizationScopes), ClientAuthenticationMode: logindomain.ClientAuthenticationSecretBasic,
	}, nil
}

// BindInput is supplied only after a trusted upstream adapter has validated the external
// assertion. The raw subject is hashed before it enters the domain or repository layer.
type BindInput struct {
	TenantID        string
	OperatorID      string
	ProviderCode    string
	UserID          string
	ExternalSubject string
}

// Bind links a verified external subject to a local user. It never returns the raw subject.
func (service *Service) Bind(ctx context.Context, input BindInput) (domain.Binding, error) {
	input = normalizeBind(input)
	if input.TenantID == "" || input.OperatorID == "" || input.ProviderCode == "" || input.UserID == "" || !validExternalSubject(input.ExternalSubject) {
		return domain.Binding{}, ErrInvalidRequest
	}
	provider, err := service.repository.FindProviderByCode(ctx, input.TenantID, input.ProviderCode)
	if err != nil {
		return domain.Binding{}, err
	}
	if provider.Status != domain.ProviderStatusActive {
		return domain.Binding{}, ErrConflict
	}
	userExists, err := service.repository.UserExists(ctx, input.TenantID, input.UserID)
	if err != nil {
		return domain.Binding{}, err
	}
	if !userExists {
		return domain.Binding{}, ErrNotFound
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("generate federated binding ID: %w", err)
	}
	binding := domain.Binding{ID: id, TenantID: input.TenantID, ProviderID: provider.ID, UserID: input.UserID, SubjectHash: subjectHash(input.ExternalSubject), BoundAt: now, Status: domain.BindingStatusActive, Version: 1}
	return service.repository.Bind(ctx, binding)
}

// ListBindings returns only metadata required for management; it never exposes a subject or its hash.
func (service *Service) ListBindings(ctx context.Context, tenantID, userID string) ([]domain.Binding, error) {
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	if tenantID == "" || userID == "" {
		return nil, ErrInvalidRequest
	}
	return service.repository.ListBindings(ctx, tenantID, userID)
}

// UnbindInput removes one active binding, not every binding the user has at a provider.
type UnbindInput struct {
	TenantID   string
	OperatorID string
	UserID     string
	BindingID  string
	Version    uint64
}

// Unbind marks exactly one matching binding as UNBOUND with optimistic locking.
func (service *Service) Unbind(ctx context.Context, input UnbindInput) (domain.Binding, error) {
	input = normalizeUnbind(input)
	if input.TenantID == "" || input.OperatorID == "" || input.UserID == "" || input.BindingID == "" || input.Version == 0 {
		return domain.Binding{}, ErrInvalidRequest
	}
	binding, err := service.repository.FindBindingByID(ctx, input.TenantID, input.BindingID)
	if err != nil {
		return domain.Binding{}, err
	}
	if binding.UserID != input.UserID || binding.Status != domain.BindingStatusActive {
		return domain.Binding{}, ErrNotFound
	}
	return service.repository.UnbindBinding(ctx, input.TenantID, input.UserID, input.BindingID, input.Version, service.clock.Now().UTC())
}

// ResolvedSubject is the local identity used by a trusted external authentication edge.
type ResolvedSubject struct {
	TenantID   string
	ProviderID string
	UserID     string
	BindingID  string
}

// ResolveExternalSubject maps a verified external subject to a local identity. Callers must have
// already validated issuer, signature, audience, nonce/state, and token lifetime.
func (service *Service) ResolveExternalSubject(ctx context.Context, tenantID, providerCode, externalSubject string) (ResolvedSubject, error) {
	tenantID = strings.TrimSpace(tenantID)
	providerCode = strings.TrimSpace(providerCode)
	externalSubject = strings.TrimSpace(externalSubject)
	if tenantID == "" || providerCode == "" || !validExternalSubject(externalSubject) {
		return ResolvedSubject{}, ErrInvalidRequest
	}
	provider, err := service.repository.FindProviderByCode(ctx, tenantID, providerCode)
	if err != nil {
		return ResolvedSubject{}, err
	}
	if provider.Status != domain.ProviderStatusActive {
		return ResolvedSubject{}, ErrNotFound
	}
	binding, err := service.repository.ResolveActiveBinding(ctx, tenantID, provider.ID, subjectHash(externalSubject))
	if err != nil {
		return ResolvedSubject{}, err
	}
	return ResolvedSubject{TenantID: tenantID, ProviderID: provider.ID, UserID: binding.UserID, BindingID: binding.ID}, nil
}

func normalizeCreateProvider(input CreateProviderInput) CreateProviderInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Code = strings.TrimSpace(input.Code)
	input.Type = strings.ToUpper(strings.TrimSpace(input.Type))
	input.Issuer = strings.TrimSpace(input.Issuer)
	input.ClientID = strings.TrimSpace(input.ClientID)
	// Preserve the exact secret bytes; surrounding whitespace can be part of an OAuth client secret.
	input.CallbackURI = strings.TrimSpace(input.CallbackURI)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.AuthorizationScopes = normalizeScopes(input.AuthorizationScopes)
	return input
}

func normalizeProviderUpdate(input ProviderUpdate) ProviderUpdate {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	if input.ClientID != nil {
		value := strings.TrimSpace(*input.ClientID)
		input.ClientID = &value
	}
	if input.CallbackURI != nil {
		value := strings.TrimSpace(*input.CallbackURI)
		input.CallbackURI = &value
	}
	// Preserve the exact secret bytes; only validation uses trimming to reject blank input.
	if input.AuthorizationScopes != nil {
		value := normalizeScopes(*input.AuthorizationScopes)
		input.AuthorizationScopes = &value
	}
	return input
}

func normalizeBind(input BindInput) BindInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ProviderCode = strings.TrimSpace(input.ProviderCode)
	input.UserID = strings.TrimSpace(input.UserID)
	input.ExternalSubject = strings.TrimSpace(input.ExternalSubject)
	return input
}

func normalizeUnbind(input UnbindInput) UnbindInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.UserID = strings.TrimSpace(input.UserID)
	input.BindingID = strings.TrimSpace(input.BindingID)
	return input
}

func normalizePageRequest(query PageRequest) PageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = defaultPageSize
	}
	if query.PageSize > maxPageSize {
		query.PageSize = maxPageSize + 1
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Status = strings.ToUpper(strings.TrimSpace(query.Status))
	return query
}

func validCreateProvider(input CreateProviderInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && validCode(input.Code, 64) &&
		validProviderConfiguration(input.Type, input.Issuer, input.ClientID, input.ClientSecret, input.CallbackURI, input.AuthorizationScopes) &&
		validDisplayName(input.DisplayName)
}

func validOptionalRuntimeConfiguration(providerType string, input ProviderUpdate) bool {
	return (input.ClientID == nil || validClientID(*input.ClientID)) &&
		(input.CallbackURI == nil || validCallbackURIForProvider(providerType, *input.CallbackURI)) &&
		(input.AuthorizationScopes == nil || validScopesForProvider(providerType, *input.AuthorizationScopes)) &&
		(input.ClientSecret == nil || validClientSecret(*input.ClientSecret))
}

func validPersistedRuntimeConfiguration(provider domain.Provider) bool {
	return validProviderConfiguration(provider.Type, provider.Issuer, provider.ClientID, "configured", provider.CallbackURI, provider.AuthorizationScopes) &&
		provider.HasClientSecret()
}

func validProviderConfiguration(providerType, issuer, clientID, clientSecret, callbackURI string, scopes []string) bool {
	if !validClientID(clientID) || !validClientSecret(clientSecret) {
		return false
	}
	switch providerType {
	case domain.ProviderTypeOIDC:
		return validIssuer(issuer) && validCallbackURI(callbackURI) && validScopes(scopes)
	case domain.ProviderTypeDingTalkQR:
		return issuer == "" && validDingTalkCallbackURI(callbackURI) && validDingTalkPermissions(scopes)
	default:
		return false
	}
}

func validCallbackURIForProvider(providerType, callbackURI string) bool {
	switch providerType {
	case domain.ProviderTypeOIDC:
		return validCallbackURI(callbackURI)
	case domain.ProviderTypeDingTalkQR:
		return validDingTalkCallbackURI(callbackURI)
	default:
		return false
	}
}

func validScopesForProvider(providerType string, scopes []string) bool {
	switch providerType {
	case domain.ProviderTypeOIDC:
		return validScopes(scopes)
	case domain.ProviderTypeDingTalkQR:
		return validDingTalkPermissions(scopes)
	default:
		return false
	}
}

func validCode(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (index > 0 && character >= '0' && character <= '9') || (index > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func validIssuer(value string) bool {
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Fragment == "" && parsed.User == nil
}

func validClientID(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value)
}
func validClientSecret(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 4096 && utf8.ValidString(value)
}

func validCallbackURI(value string) bool {
	parsed, ok := parseCallbackURI(value)
	if !ok {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	host := parsed.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil
}

// validDingTalkCallbackURI accepts only an HTTPS callback URI. QR-code callbacks leave the
// embedded login component and complete the platform session in the top-level browser context.
func validDingTalkCallbackURI(value string) bool {
	parsed, ok := parseCallbackURI(value)
	return ok && parsed.Scheme == "https"
}

func parseCallbackURI(value string) (*url.URL, bool) {
	if len(value) == 0 || len(value) > 2048 {
		return nil, false
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.Fragment != "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, false
	}
	return parsed, true
}

func normalizeScopes(scopes []string) []string {
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		result = append(result, strings.TrimSpace(scope))
	}
	return result
}

func validScopes(scopes []string) bool {
	return validScopeValues(scopes, true)
}

// validDingTalkPermissions validates the configured DingTalk permission names. The provider
// does not use the OIDC openid scope, but at least one upstream permission must be explicit.
func validDingTalkPermissions(permissions []string) bool {
	return validScopeValues(permissions, false)
}

func validScopeValues(scopes []string, requireOpenID bool) bool {
	if len(scopes) == 0 || len(scopes) > maxScopes {
		return false
	}
	seen := make(map[string]struct{}, len(scopes))
	hasOpenID := false
	for _, scope := range scopes {
		if scope == "" || len(scope) > 128 {
			return false
		}
		for _, character := range scope {
			if character <= 0x20 || character == 0x7f {
				return false
			}
		}
		if _, exists := seen[scope]; exists {
			return false
		}
		seen[scope] = struct{}{}
		hasOpenID = hasOpenID || scope == "openid"
	}
	return !requireOpenID || hasOpenID
}

func validDisplayName(value string) bool { return value != "" && utf8.RuneCountInString(value) <= 128 }
func validProviderStatus(value string) bool {
	return value == domain.ProviderStatusActive || value == domain.ProviderStatusDisabled
}
func validProviderStatusFilter(value string) bool { return value == "" || validProviderStatus(value) }
func validExternalSubject(value string) bool {
	return value != "" && len(value) <= 2048 && utf8.ValidString(value)
}
func subjectHash(externalSubject string) [32]byte { return sha256.Sum256([]byte(externalSubject)) }

func cloneStrings(values []string) []string { return append([]string(nil), values...) }
func cloneBytes(value []byte) []byte        { return append([]byte(nil), value...) }
