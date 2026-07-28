// Package application provides OAuth client registration and credential lifecycle use cases.
package application

import (
	"context"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrManagementNotFound indicates a tenant-scoped OAuth client or credential was not found.
	ErrManagementNotFound = errors.New("OAuth client resource not found")
	// ErrManagementConflict indicates a duplicate client id or a stale aggregate version.
	ErrManagementConflict = errors.New("OAuth client resource conflict")
	// ErrManagementValidation indicates caller-correctable registration input.
	ErrManagementValidation = errors.New("invalid OAuth client management input")
)

const (
	oauthClientStatusActive   = "ACTIVE"
	oauthClientStatusDisabled = "DISABLED"
	credentialStatusActive    = "ACTIVE"
	credentialStatusRevoked   = "REVOKED"

	clientSecretPrefix      = "ocsec_"
	clientSecretRandomBytes = 32
	maxClientSecretLifetime = 365 * 24 * time.Hour
	maxRotationOverlap      = 30 * 24 * time.Hour
)

var (
	clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,128}$`)
	scopePattern    = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
)

// ManagementIdentifierGenerator supplies aggregate and credential identifiers.
type ManagementIdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// OAuthClientManagementRepository persists OAuth client aggregate changes. Implementations must
// make each write atomic, including replacement of aggregate child collections and credential
// rotation.
type OAuthClientManagementRepository interface {
	CreateOAuthClient(context.Context, OAuthClientCreateInput, string, *SecretWrite, time.Time) (OAuthClientView, error)
	ListOAuthClients(context.Context, string) ([]OAuthClientView, error)
	GetOAuthClient(context.Context, string, string) (OAuthClientView, error)
	ReplaceOAuthClientScopes(context.Context, OAuthClientScopesUpdateInput, time.Time) (OAuthClientView, error)
	ReplaceOAuthClientRedirectURIs(context.Context, OAuthClientRedirectURIsUpdateInput, time.Time) (OAuthClientView, error)
	GetOAuthClientPostLogoutRedirectURIs(context.Context, string, string) (OAuthClientPostLogoutRedirectURIsView, error)
	ReplaceOAuthClientPostLogoutRedirectURIs(context.Context, OAuthClientPostLogoutRedirectURIsUpdateInput, time.Time) (OAuthClientPostLogoutRedirectURIsView, error)
	GetOAuthClientJWKs(context.Context, string, string) (OAuthClientJWKsView, error)
	ReplaceOAuthClientJWKs(context.Context, OAuthClientJWKsUpdateInput, time.Time) (OAuthClientJWKsView, error)
	DisableOAuthClient(context.Context, OAuthClientDisableInput, time.Time) (OAuthClientView, error)
	CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput, SecretWrite, time.Time) (OAuthClientCredentialView, error)
	RotateOAuthClientSecret(context.Context, OAuthClientSecretRotateInput, SecretWrite, time.Time) (OAuthClientCredentialView, error)
	DisableOAuthClientSecret(context.Context, OAuthClientSecretDisableInput, time.Time) error
}

// OAuthClientCreateInput defines a tenant-scoped client registration. A client using
// client_secret_basic receives one generated secret in the create result; it is never stored in
// this input or in a normal OAuthClientView.
type OAuthClientCreateInput struct {
	TenantID               string
	ApplicationID          string
	EnvironmentID          string
	OperatorID             string
	ClientID               string
	ClientName             string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	GrantTypes             []string
	Scopes                 []string
	RedirectURIs           []string
	SecretValidUntil       *time.Time
}

// OAuthClientScopesUpdateInput replaces every allowed scope using optimistic locking.
type OAuthClientScopesUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	Scopes        []string
	Version       uint64
}

// OAuthClientRedirectURIsUpdateInput replaces every callback URL using optimistic locking.
type OAuthClientRedirectURIsUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	RedirectURIs  []string
	Version       uint64
}

// OAuthClientPostLogoutRedirectURIsUpdateInput 使用乐观锁整体替换 RP 发起登出后的回调地址集合。
// 该集合与 OAuth 授权回调地址独立，避免两类用途混用。
type OAuthClientPostLogoutRedirectURIsUpdateInput struct {
	TenantID               string
	OAuthClientID          string
	OperatorID             string
	PostLogoutRedirectURIs []string
	Version                uint64
}

// OAuthClientJWK 表示用于 private_key_jwt 客户端断言或签名授权请求的单个公钥。
// PublicJWK 只能包含一个公钥 JWK 对象；应用服务会在持久化前拒绝所有私钥字段。
type OAuthClientJWK struct {
	ID         string
	KeyID      string
	PublicJWK  json.RawMessage
	Algorithm  string
	ValidFrom  time.Time
	ValidUntil *time.Time
	Status     string
}

// OAuthClientJWKsUpdateInput 使用乐观锁整体替换客户端公钥 JWK 集合。
// JWKs 中的 ID 由服务端生成，调用方提供的值会被覆盖。
type OAuthClientJWKsUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	JWKs          []OAuthClientJWK
	Version       uint64
}

// OAuthClientPostLogoutRedirectURIsView 表示独立的登出后回调地址集合，
// 并携带后续整体替换所需的聚合版本号。
type OAuthClientPostLogoutRedirectURIsView struct {
	OAuthClientID          string
	Version                uint64
	PostLogoutRedirectURIs []string
}

// OAuthClientJWKsView 表示安全的公钥集合，并携带后续整体替换所需的聚合版本号。
type OAuthClientJWKsView struct {
	OAuthClientID string
	Version       uint64
	JWKs          []OAuthClientJWK
}

// OAuthClientDisableInput disables a client and revokes all its active secrets.
type OAuthClientDisableInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	Version       uint64
}

// OAuthClientSecretCreateInput creates an additional concurrently active secret.
type OAuthClientSecretCreateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	ValidUntil    *time.Time
}

// OAuthClientSecretRotateInput creates a new secret and keeps currently active secrets valid
// only through the requested bounded overlap window. An overlap of zero revokes them immediately.
type OAuthClientSecretRotateInput struct {
	TenantID       string
	OAuthClientID  string
	OperatorID     string
	OverlapSeconds uint
	ValidUntil     *time.Time
}

// OAuthClientSecretDisableInput revokes one credential version without exposing its hash.
type OAuthClientSecretDisableInput struct {
	TenantID      string
	OAuthClientID string
	CredentialID  string
	OperatorID    string
}

// SecretWrite is the persistence-safe representation of a generated secret. PlaintextSecret is
// intentionally separate and must never be passed to a repository or logged.
type SecretWrite struct {
	CredentialID string
	SecretHash   []byte
	Fingerprint  string
	ValidUntil   *time.Time
}

// OAuthClientCredentialView contains safe secret-version metadata only.
type OAuthClientCredentialView struct {
	ID          string
	Fingerprint string
	ValidFrom   time.Time
	ValidUntil  *time.Time
	RevokedAt   *time.Time
	Status      string
}

// OAuthClientView is a safe management projection. It deliberately has no secret hash or secret
// plaintext fields.
type OAuthClientView struct {
	ID                     string
	TenantID               string
	ApplicationID          string
	EnvironmentID          string
	ClientID               string
	ClientName             string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	Status                 string
	Version                uint64
	GrantTypes             []string
	Scopes                 []string
	RedirectURIs           []string
	Credentials            []OAuthClientCredentialView
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// OAuthClientSecretResult is returned only by creation and secret lifecycle operations. Its
// PlaintextSecret must be serialized exactly once by the corresponding HTTP response and never
// logged, audited, cached, or persisted.
type OAuthClientSecretResult struct {
	Credential      OAuthClientCredentialView
	PlaintextSecret string
}

// OAuthClientCreateResult returns a safe registration projection plus the initial secret, when
// the selected authentication method requires one.
type OAuthClientCreateResult struct {
	Client          OAuthClientView
	PlaintextSecret string
}

// RedirectURIValidationPolicy controls which OAuth client callback schemes may be registered.
// HTTPS is always allowed. HTTP is limited to loopback by default and may be enabled for
// trusted server deployments through AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS.
type RedirectURIValidationPolicy struct {
	AllowInsecureHTTP bool
}

// OAuthClientManagementService coordinates validation, cryptographic secret generation, hashing,
// and tenant-scoped persistence for OAuth client registrations.
type OAuthClientManagementService struct {
	repository                  OAuthClientManagementRepository
	ids                         ManagementIdentifierGenerator
	clock                       Clock
	redirectURIValidationPolicy RedirectURIValidationPolicy
}

// NewOAuthClientManagementService constructs the management service.
func NewOAuthClientManagementService(repository OAuthClientManagementRepository, ids ManagementIdentifierGenerator, clock Clock, redirectURIValidationPolicy RedirectURIValidationPolicy) (*OAuthClientManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("OAuth client management service dependencies must not be nil")
	}
	return &OAuthClientManagementService{
		repository: repository, ids: ids, clock: clock, redirectURIValidationPolicy: redirectURIValidationPolicy,
	}, nil
}

// CreateOAuthClient registers an OAuth client and creates its initial secret only for
// client_secret_basic registrations.
func (service *OAuthClientManagementService) CreateOAuthClient(ctx context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	input, err := normalizeOAuthClientCreate(input, service.redirectURIValidationPolicy)
	if err != nil {
		return OAuthClientCreateResult{}, err
	}
	now := service.clock.Now().UTC()
	if !validSecretValidUntil(input.SecretValidUntil, now) {
		return OAuthClientCreateResult{}, ErrManagementValidation
	}
	clientID, err := service.ids.New(now)
	if err != nil {
		return OAuthClientCreateResult{}, fmt.Errorf("generate OAuth client id: %w", err)
	}

	var secret *SecretWrite
	var plaintext string
	if input.TokenAuthMethod == "client_secret_basic" {
		write, value, err := service.newSecretWrite(now, input.SecretValidUntil)
		if err != nil {
			return OAuthClientCreateResult{}, err
		}
		secret = &write
		plaintext = value
	}

	client, err := service.repository.CreateOAuthClient(ctx, input, clientID, secret, now)
	if err != nil {
		return OAuthClientCreateResult{}, err
	}
	return OAuthClientCreateResult{Client: client, PlaintextSecret: plaintext}, nil
}

// ListOAuthClients returns safe OAuth client projections for one tenant.
func (service *OAuthClientManagementService) ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, ErrManagementValidation
	}
	return service.repository.ListOAuthClients(ctx, tenantID)
}

// GetOAuthClient returns a safe tenant-scoped client projection.
func (service *OAuthClientManagementService) GetOAuthClient(ctx context.Context, tenantID, oauthClientID string) (OAuthClientView, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(oauthClientID) == "" {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClient(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID))
}

// ReplaceOAuthClientScopes replaces all allowed scopes for a client.
func (service *OAuthClientManagementService) ReplaceOAuthClientScopes(ctx context.Context, input OAuthClientScopesUpdateInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.Scopes = normalizeStrings(input.Scopes)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validScopes(input.Scopes) {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientScopes(ctx, input, service.clock.Now().UTC())
}

// ReplaceOAuthClientRedirectURIs replaces all registered callback URLs for a client.
func (service *OAuthClientManagementService) ReplaceOAuthClientRedirectURIs(ctx context.Context, input OAuthClientRedirectURIsUpdateInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.RedirectURIs = normalizeStrings(input.RedirectURIs)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validRedirectURIs(input.RedirectURIs, service.redirectURIValidationPolicy) {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientRedirectURIs(ctx, input, service.clock.Now().UTC())
}

// GetOAuthClientPostLogoutRedirectURIs 在租户边界内读取独立的登出后回调地址集合。
func (service *OAuthClientManagementService) GetOAuthClientPostLogoutRedirectURIs(ctx context.Context, tenantID, oauthClientID string) (OAuthClientPostLogoutRedirectURIsView, error) {
	tenantID, oauthClientID = strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID)
	if tenantID == "" || oauthClientID == "" {
		return OAuthClientPostLogoutRedirectURIsView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClientPostLogoutRedirectURIs(ctx, tenantID, oauthClientID)
}

// ReplaceOAuthClientPostLogoutRedirectURIs 使用乐观锁整体替换独立的登出后回调地址集合。
func (service *OAuthClientManagementService) ReplaceOAuthClientPostLogoutRedirectURIs(ctx context.Context, input OAuthClientPostLogoutRedirectURIsUpdateInput) (OAuthClientPostLogoutRedirectURIsView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.PostLogoutRedirectURIs = normalizeStrings(input.PostLogoutRedirectURIs)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validRedirectURIs(input.PostLogoutRedirectURIs, service.redirectURIValidationPolicy) {
		return OAuthClientPostLogoutRedirectURIsView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientPostLogoutRedirectURIs(ctx, input, service.clock.Now().UTC())
}

// GetOAuthClientJWKs 在租户边界内读取已登记的公钥材料。
func (service *OAuthClientManagementService) GetOAuthClientJWKs(ctx context.Context, tenantID, oauthClientID string) (OAuthClientJWKsView, error) {
	tenantID, oauthClientID = strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID)
	if tenantID == "" || oauthClientID == "" {
		return OAuthClientJWKsView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClientJWKs(ctx, tenantID, oauthClientID)
}

// ReplaceOAuthClientJWKs 整体替换供 private_key_jwt 和 JAR 使用的公钥 JWK。
// 服务会生成所有持久化标识，并在调用仓储前拒绝私钥材料。
func (service *OAuthClientManagementService) ReplaceOAuthClientJWKs(ctx context.Context, input OAuthClientJWKsUpdateInput) (OAuthClientJWKsView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 {
		return OAuthClientJWKsView{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	keys, err := normalizeOAuthClientJWKs(input.JWKs, now, service.ids)
	if err != nil {
		return OAuthClientJWKsView{}, err
	}
	input.JWKs = keys
	return service.repository.ReplaceOAuthClientJWKs(ctx, input, now)
}

// DisableOAuthClient disables the aggregate and revokes every currently active client secret.
func (service *OAuthClientManagementService) DisableOAuthClient(ctx context.Context, input OAuthClientDisableInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.DisableOAuthClient(ctx, input, service.clock.Now().UTC())
}

// CreateOAuthClientSecret adds a new active secret. It is restricted to active
// client_secret_basic clients by the repository's guarded write.
func (service *OAuthClientManagementService) CreateOAuthClientSecret(ctx context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	validUntil := normalizeSecretValidUntil(input.ValidUntil)
	if !validSecretValidUntil(validUntil, now) {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	write, plaintext, err := service.newSecretWrite(now, validUntil)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	credential, err := service.repository.CreateOAuthClientSecret(ctx, input, write, now)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	return OAuthClientSecretResult{Credential: credential, PlaintextSecret: plaintext}, nil
}

// RotateOAuthClientSecret creates a new active secret and limits old active secrets to a bounded
// overlap period, allowing consumers to deploy the new secret without a gap.
func (service *OAuthClientManagementService) RotateOAuthClientSecret(ctx context.Context, input OAuthClientSecretRotateInput) (OAuthClientSecretResult, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || time.Duration(input.OverlapSeconds)*time.Second > maxRotationOverlap {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	validUntil := normalizeSecretValidUntil(input.ValidUntil)
	if !validSecretValidUntil(validUntil, now) {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	write, plaintext, err := service.newSecretWrite(now, validUntil)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	credential, err := service.repository.RotateOAuthClientSecret(ctx, input, write, now)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	return OAuthClientSecretResult{Credential: credential, PlaintextSecret: plaintext}, nil
}

// DisableOAuthClientSecret immediately revokes one secret version.
func (service *OAuthClientManagementService) DisableOAuthClientSecret(ctx context.Context, input OAuthClientSecretDisableInput) error {
	input.TenantID, input.OAuthClientID, input.CredentialID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.CredentialID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.CredentialID == "" || input.OperatorID == "" {
		return ErrManagementValidation
	}
	return service.repository.DisableOAuthClientSecret(ctx, input, service.clock.Now().UTC())
}

func (service *OAuthClientManagementService) newSecretWrite(now time.Time, validUntil *time.Time) (SecretWrite, string, error) {
	return newOAuthClientSecretWrite(service.ids, now, validUntil)
}

// newOAuthClientSecretWrite centralizes secret generation for both standalone OAuth client
// management and the atomic subsystem-onboarding workflow.
func newOAuthClientSecretWrite(ids ManagementIdentifierGenerator, now time.Time, validUntil *time.Time) (SecretWrite, string, error) {
	credentialID, err := ids.New(now)
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("generate OAuth client credential id: %w", err)
	}
	plaintext, err := generateClientSecret()
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("generate OAuth client secret: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("hash OAuth client secret: %w", err)
	}
	return SecretWrite{CredentialID: credentialID, SecretHash: hash, Fingerprint: secretFingerprint(plaintext), ValidUntil: validUntil}, plaintext, nil
}

func normalizeOAuthClientCreate(input OAuthClientCreateInput, redirectURIValidationPolicy RedirectURIValidationPolicy) (OAuthClientCreateInput, error) {
	input.TenantID, input.ApplicationID, input.EnvironmentID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.ApplicationID), strings.TrimSpace(input.EnvironmentID), strings.TrimSpace(input.OperatorID)
	input.ClientID, input.ClientName = strings.TrimSpace(input.ClientID), strings.TrimSpace(input.ClientName)
	input.ClientType, input.TokenAuthMethod = strings.TrimSpace(input.ClientType), strings.TrimSpace(input.TokenAuthMethod)
	input.GrantTypes, input.Scopes, input.RedirectURIs = normalizeStrings(input.GrantTypes), normalizeStrings(input.Scopes), normalizeStrings(input.RedirectURIs)
	input.SecretValidUntil = normalizeSecretValidUntil(input.SecretValidUntil)
	if input.TenantID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.OperatorID == "" ||
		!clientIDPattern.MatchString(input.ClientID) || !validOAuthClientName(input.ClientName) ||
		!validClientConfiguration(input) || !validScopes(input.Scopes) || !validRedirectURIs(input.RedirectURIs, redirectURIValidationPolicy) {
		return OAuthClientCreateInput{}, ErrManagementValidation
	}
	return input, nil
}

func validClientConfiguration(input OAuthClientCreateInput) bool {
	if !oneOf(input.ClientType, "public", "confidential", "service") || !oneOf(input.TokenAuthMethod, "none", "client_secret_basic", "private_key_jwt") ||
		input.AccessTokenTTLSeconds < 60 || input.AccessTokenTTLSeconds > 86400 || input.RefreshTokenTTLSeconds > 30*24*60*60 || !validGrantTypes(input.GrantTypes) {
		return false
	}
	hasAuthorizationCode, hasClientCredentials := contains(input.GrantTypes, "authorization_code"), contains(input.GrantTypes, "client_credentials")
	if hasAuthorizationCode && (!input.RequirePKCE || len(input.RedirectURIs) == 0) {
		return false
	}
	if hasClientCredentials && (input.ClientType == "public" || !oneOf(input.TokenAuthMethod, "client_secret_basic", "private_key_jwt")) {
		return false
	}
	if input.TokenAuthMethod == "none" && input.ClientType != "public" {
		return false
	}
	if (input.TokenAuthMethod == "client_secret_basic" || input.TokenAuthMethod == "private_key_jwt") && input.ClientType == "public" {
		return false
	}
	if input.TokenAuthMethod != "client_secret_basic" && input.SecretValidUntil != nil {
		return false
	}
	return true
}

func validGrantTypes(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !oneOf(value, "authorization_code", "client_credentials", "refresh_token") {
			return false
		}
	}
	return true
}

func validScopes(values []string) bool {
	for _, value := range values {
		if !scopePattern.MatchString(value) {
			return false
		}
	}
	return true
}

func validRedirectURIs(values []string, policy RedirectURIValidationPolicy) bool {
	for _, value := range values {
		if len(value) > 2048 || !validRedirectURI(value, policy) {
			return false
		}
	}
	return true
}

// validRedirectURI accepts an absolute HTTP(S) callback address. The authorization endpoint still
// requires an exact match with the URI registered for the OAuth client, so this does not turn the
// callback target into an open redirect. HTTP callback URLs outside loopback require explicit
// process configuration because authorization codes must otherwise traverse an insecure channel.
func validRedirectURI(value string, policy RedirectURIValidationPolicy) bool {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	if policy.AllowInsecureHTTP {
		return true
	}
	host := parsed.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeSecretValidUntil(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func validSecretValidUntil(value *time.Time, now time.Time) bool {
	return value == nil || (value.After(now) && value.Sub(now) <= maxClientSecretLifetime)
}

func validOAuthClientName(value string) bool {
	length := utf8.RuneCountInString(value)
	return length >= 1 && length <= 128
}

func generateClientSecret() (string, error) {
	bytes := make([]byte, clientSecretRandomBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return clientSecretPrefix + base64.RawURLEncoding.EncodeToString(bytes), nil
}

func secretFingerprint(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var privateJWKMembers = []string{"d", "k", "p", "q", "dp", "dq", "qi", "oth"}

var allowedPublicJWKMembers = map[string]struct{}{
	"alg": {}, "crv": {}, "e": {}, "kid": {}, "kty": {}, "n": {}, "use": {}, "x": {}, "y": {},
}

// normalizeOAuthClientJWKs 校验并规范化完整的公钥 JWK 集合。
// 它保留未来生效的轮换密钥，同时拒绝格式错误和值中包含的私钥材料。
func normalizeOAuthClientJWKs(values []OAuthClientJWK, now time.Time, ids ManagementIdentifierGenerator) ([]OAuthClientJWK, error) {
	result := make([]OAuthClientJWK, 0, len(values))
	seenKeyIDs := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.KeyID = strings.TrimSpace(value.KeyID)
		value.Algorithm = strings.TrimSpace(value.Algorithm)
		if !validOAuthClientJWKKeyID(value.KeyID) {
			return nil, ErrManagementValidation
		}
		if _, exists := seenKeyIDs[value.KeyID]; exists {
			return nil, ErrManagementValidation
		}
		seenKeyIDs[value.KeyID] = struct{}{}

		validFrom := value.ValidFrom.UTC()
		if value.ValidFrom.IsZero() {
			validFrom = now
		}
		validUntil := normalizeSecretValidUntil(value.ValidUntil)
		if validUntil != nil && !validUntil.After(validFrom) {
			return nil, ErrManagementValidation
		}
		publicJWK, algorithm, err := validateAndCanonicalizePublicJWK(value.PublicJWK, value.KeyID, value.Algorithm)
		if err != nil {
			return nil, ErrManagementValidation
		}
		id, err := ids.New(now)
		if err != nil {
			return nil, fmt.Errorf("generate OAuth client JWK id: %w", err)
		}
		result = append(result, OAuthClientJWK{ID: id, KeyID: value.KeyID, PublicJWK: publicJWK, Algorithm: algorithm, ValidFrom: validFrom, ValidUntil: validUntil, Status: credentialStatusActive})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].KeyID < result[j].KeyID })
	return result, nil
}

func validOAuthClientJWKKeyID(value string) bool {
	if value == "" || len(value) > 255 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validateAndCanonicalizePublicJWK(raw json.RawMessage, expectedKeyID, requestedAlgorithm string) (json.RawMessage, string, error) {
	var values map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, "", errors.New("invalid JWK")
	}
	for member := range values {
		if contains(privateJWKMembers, member) {
			return nil, "", errors.New("private JWK member")
		}
		if _, allowed := allowedPublicJWKMembers[member]; !allowed {
			return nil, "", errors.New("unsupported JWK member")
		}
	}
	keyID, ok := jwkString(values, "kid")
	if !ok || keyID != expectedKeyID {
		return nil, "", errors.New("JWK kid mismatch")
	}
	keyType, ok := jwkString(values, "kty")
	if !ok {
		return nil, "", errors.New("missing JWK kty")
	}
	registeredAlgorithm, hasRegisteredAlgorithm := jwkString(values, "alg")
	if hasRegisteredAlgorithm && !supportedOAuthClientJWKAlgorithm(registeredAlgorithm) {
		return nil, "", errors.New("unsupported JWK algorithm")
	}
	if requestedAlgorithm != "" && !supportedOAuthClientJWKAlgorithm(requestedAlgorithm) {
		return nil, "", errors.New("unsupported requested algorithm")
	}
	if requestedAlgorithm != "" && hasRegisteredAlgorithm && requestedAlgorithm != registeredAlgorithm {
		return nil, "", errors.New("JWK algorithm mismatch")
	}
	algorithm := requestedAlgorithm
	if algorithm == "" {
		algorithm = registeredAlgorithm
	}
	if err := validateOAuthClientPublicJWK(keyType, values, algorithm); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(values)
	if err != nil {
		return nil, "", err
	}
	return json.RawMessage(canonical), algorithm, nil
}

func validateOAuthClientPublicJWK(keyType string, values map[string]json.RawMessage, algorithm string) error {
	value := func(name string) (string, bool) { return jwkString(values, name) }
	decode := func(encoded string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(encoded) }
	compatible := func(allowed ...string) bool { return algorithm == "" || oneOf(algorithm, allowed...) }
	switch keyType {
	case "OKP":
		curve, hasCurve := value("crv")
		x, hasX := value("x")
		if !hasCurve || curve != "Ed25519" || !hasX || !compatible("EdDSA") {
			return errors.New("invalid OKP JWK")
		}
		bytes, err := decode(x)
		if err != nil || len(bytes) != ed25519.PublicKeySize {
			return errors.New("invalid Ed25519 key")
		}
	case "RSA":
		modulus, hasModulus := value("n")
		exponent, hasExponent := value("e")
		if !hasModulus || !hasExponent || !compatible("RS256", "RS384", "RS512", "PS256", "PS384", "PS512") {
			return errors.New("invalid RSA JWK")
		}
		modulusBytes, modulusErr := decode(modulus)
		exponentBytes, exponentErr := decode(exponent)
		exponentNumber := new(big.Int).SetBytes(exponentBytes)
		if modulusErr != nil || exponentErr != nil || len(modulusBytes) < 256 || !exponentNumber.IsInt64() || exponentNumber.Int64() < 3 || exponentNumber.Int64()%2 == 0 {
			return errors.New("invalid RSA key")
		}
	case "EC":
		curveName, hasCurve := value("crv")
		x, hasX := value("x")
		y, hasY := value("y")
		curve, coordinateLength, expectedAlgorithm := oauthClientJWKCurve(curveName)
		if !hasCurve || !hasX || !hasY || curve == nil || (algorithm != "" && algorithm != expectedAlgorithm) {
			return errors.New("invalid EC JWK")
		}
		xBytes, xErr := decode(x)
		yBytes, yErr := decode(y)
		publicX, publicY := new(big.Int).SetBytes(xBytes), new(big.Int).SetBytes(yBytes)
		if xErr != nil || yErr != nil || len(xBytes) != coordinateLength || len(yBytes) != coordinateLength || !curve.IsOnCurve(publicX, publicY) {
			return errors.New("invalid EC key")
		}
	default:
		return errors.New("unsupported JWK")
	}
	return nil
}

func oauthClientJWKCurve(name string) (elliptic.Curve, int, string) {
	switch name {
	case "P-256":
		return elliptic.P256(), 32, "ES256"
	case "P-384":
		return elliptic.P384(), 48, "ES384"
	case "P-521":
		return elliptic.P521(), 66, "ES512"
	default:
		return nil, 0, ""
	}
}

func supportedOAuthClientJWKAlgorithm(value string) bool {
	return oneOf(value, "EdDSA", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512")
}

func jwkString(values map[string]json.RawMessage, name string) (string, bool) {
	raw, exists := values[name]
	if !exists {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}
