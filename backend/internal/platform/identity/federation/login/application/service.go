// Package application coordinates an external OpenID Connect authorization-code browser login.
package application

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

var (
	// ErrInvalidRequest identifies a malformed browser-login request before any upstream call.
	ErrInvalidRequest = errors.New("invalid external login request")
	// ErrProviderUnavailable deliberately covers absent, disabled and unusable provider configurations.
	ErrProviderUnavailable = errors.New("external identity provider is unavailable")
	// ErrInvalidState deliberately covers expired, consumed and unknown authorization state.
	ErrInvalidState = errors.New("external login state is invalid")
	// ErrAuthorizationDenied identifies an explicit denial sent by the upstream provider.
	ErrAuthorizationDenied = errors.New("external authorization was denied")
	// ErrTokenValidation deliberately covers exchange, discovery, JWKS and ID-token validation failures.
	ErrTokenValidation = errors.New("external identity token validation failed")
	// ErrAccountNotBound means the verified upstream subject has no active local account binding.
	ErrAccountNotBound = errors.New("external identity is not bound to a local account")
	// ErrSessionIssue means a verified identity could not create a local browser session.
	ErrSessionIssue = errors.New("external login session could not be created")
)

const (
	defaultStateTTL            = 5 * time.Minute
	defaultClockSkew           = time.Minute
	maxAuthorizationCodeLength = 4096
)

// Clock makes expiry and ID-token time validation deterministic in tests.
type Clock interface{ Now() time.Time }

// SystemClock supplies UTC timestamps for production usage.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// SecretGenerator creates high-entropy, URL-safe values for state, nonce and PKCE verifier.
type SecretGenerator interface{ NewSecret() (string, error) }

// ProviderResolver supplies tenant-scoped runtime configuration. It may combine the local federation
// control plane with a secure secret source; ClientSecret must not be persisted by this package.
type ProviderResolver interface {
	ResolveProvider(context.Context, string, string) (domain.Provider, error)
}

// AccountResolver maps a cryptographically verified external subject to an active local account.
type AccountResolver interface {
	ResolveAccount(context.Context, string, string, string) (domain.LocalAccount, error)
}

// SessionIssuer creates a trusted local authentication outcome from a verified local identity intent.
// The returned BrowserSession is either a completed browser session or an MFA pre-authentication
// challenge; the issuer must never turn an MFA challenge into a completed session.
type SessionIssuer interface {
	IssueBrowserSession(context.Context, domain.SessionIssue) (domain.BrowserSession, error)
}

// StateStore atomically saves and consumes short-lived authorization state. Consume must make a state
// unavailable to all future callers regardless of whether later token validation succeeds.
type StateStore interface {
	Save(context.Context, domain.State) error
	Consume(context.Context, [32]byte, time.Time) (domain.State, error)
}

// OIDCRemote performs outbound protocol calls. A fake can be supplied to keep application tests
// deterministic; the HTTP implementation belongs to the infrastructure package.
type OIDCRemote interface {
	Discover(context.Context, string) (domain.Discovery, error)
	ExchangeAuthorizationCode(context.Context, domain.TokenExchange) (domain.TokenResponse, error)
	FetchJWKSet(context.Context, string) (domain.JWKSet, error)
}

// Config bounds browser authorization state and JWT time tolerance.
type Config struct {
	StateTTL          time.Duration
	ClockSkew         time.Duration
	AllowInsecureHTTP bool
}

// Service executes provider redirects and authenticated callbacks without depending on Gin, GORM or
// the password-login service.
type Service struct {
	providers ProviderResolver
	accounts  AccountResolver
	sessions  SessionIssuer
	states    StateStore
	secrets   SecretGenerator
	remote    OIDCRemote
	clock     Clock
	config    Config
}

// NewService validates the externally supplied adapters.
func NewService(providers ProviderResolver, accounts AccountResolver, sessions SessionIssuer, states StateStore, secrets SecretGenerator, remote OIDCRemote, clock Clock, config Config) (*Service, error) {
	if providers == nil || accounts == nil || sessions == nil || states == nil || secrets == nil || remote == nil || clock == nil {
		return nil, errors.New("external login dependencies must not be nil")
	}
	if config.StateTTL == 0 {
		config.StateTTL = defaultStateTTL
	}
	if config.ClockSkew == 0 {
		config.ClockSkew = defaultClockSkew
	}
	if config.StateTTL <= 0 || config.StateTTL > 15*time.Minute || config.ClockSkew < 0 || config.ClockSkew > 5*time.Minute {
		return nil, errors.New("external login configuration is invalid")
	}
	return &Service{providers: providers, accounts: accounts, sessions: sessions, states: states, secrets: secrets, remote: remote, clock: clock, config: config}, nil
}

// BeginInput identifies the configured tenant/provider, a validated local post-login path and the
// opaque browser binding generated by the HTTP boundary. BrowserBinding is hashed before state is
// persisted and must never be logged or returned in an application response.
type BeginInput struct {
	TenantID       string
	ProviderCode   string
	ReturnTo       string
	BrowserBinding string
}

// BeginResult is the browser redirect destination. It does not expose any local secret.
type BeginResult struct {
	AuthorizationURL string
	ExpiresAt        time.Time
}

// Begin creates a one-time state record and builds an OIDC Authorization Code + PKCE redirect.
func (service *Service) Begin(ctx context.Context, input BeginInput) (BeginResult, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.ProviderCode = strings.TrimSpace(input.ProviderCode)
	input.ReturnTo = normalizeReturnTo(input.ReturnTo)
	input.BrowserBinding = strings.TrimSpace(input.BrowserBinding)
	if input.TenantID == "" || input.ProviderCode == "" || input.ReturnTo == "" ||
		!validOpaqueValue(input.BrowserBinding, 32, 512) {
		return BeginResult{}, ErrInvalidRequest
	}

	provider, err := service.providers.ResolveProvider(ctx, input.TenantID, input.ProviderCode)
	if err != nil || !validProvider(provider, input.TenantID, input.ProviderCode, service.config.AllowInsecureHTTP) {
		return BeginResult{}, ErrProviderUnavailable
	}
	discovery, err := service.remote.Discover(ctx, provider.Issuer)
	if err != nil || !validDiscovery(discovery, provider.Issuer, service.config.AllowInsecureHTTP) {
		return BeginResult{}, ErrProviderUnavailable
	}

	stateValue, nonce, verifier, err := service.newProtocolSecrets()
	if err != nil {
		return BeginResult{}, ErrProviderUnavailable
	}
	now := service.clock.Now().UTC().Truncate(time.Second)
	state := domain.State{
		StateHash: sha256.Sum256([]byte(stateValue)), BrowserBindingHash: sha256.Sum256([]byte(input.BrowserBinding)),
		TenantID: provider.TenantID, ProviderCode: provider.Code,
		Issuer: provider.Issuer, ClientID: provider.ClientID, RedirectURI: provider.RedirectURI, Discovery: discovery,
		Nonce: nonce, PKCEVerifier: verifier, ReturnTo: input.ReturnTo, CreatedAt: now, ExpiresAt: now.Add(service.config.StateTTL),
	}
	if err := service.states.Save(ctx, state); err != nil {
		return BeginResult{}, ErrProviderUnavailable
	}

	location, err := authorizationURL(discovery.AuthorizationEndpoint, provider, stateValue, nonce, verifier)
	if err != nil {
		return BeginResult{}, ErrProviderUnavailable
	}
	return BeginResult{AuthorizationURL: location, ExpiresAt: state.ExpiresAt}, nil
}

// CallbackInput is the transport-safe upstream callback data. BrowserBinding is the opaque value
// from the short-lived HttpOnly correlation cookie. AuthorizationCode, ProviderError and
// BrowserBinding must never be recorded in logs or audit payloads.
type CallbackInput struct {
	State             string
	AuthorizationCode string
	ProviderError     string
	BrowserBinding    string
	IPAddress         net.IP
	UserAgent         string
}

// CallbackResult contains the trusted local authentication outcome and local redirect path.
type CallbackResult struct {
	Session    domain.BrowserSession
	RedirectTo string
}

// CallbackLifecycle contains only trusted local identifiers that an outer adapter may use for audit
// lifecycle events. It deliberately excludes state, authorization codes, upstream tokens and the
// upstream subject. The tenant and provider context is returned as soon as a valid state is consumed;
// binding, user and account identifiers are populated only after a verified subject resolves locally.
type CallbackLifecycle struct {
	TenantID     string
	ProviderCode string
	ProviderID   string
	BindingID    string
	UserID       string
	AccountID    string
}

// CompleteCallback preserves the original application API for callers that need only the browser
// authentication result. Callers that must produce lifecycle audit events should use
// CompleteCallbackWithLifecycle.
func (service *Service) CompleteCallback(ctx context.Context, input CallbackInput) (CallbackResult, error) {
	result, _, err := service.CompleteCallbackWithLifecycle(ctx, input)
	return result, err
}

// CompleteCallbackWithLifecycle consumes state, exchanges the code, validates the upstream ID token,
// resolves the local account binding and creates a trusted local authentication outcome. It returns
// only safe local lifecycle identifiers, including on failures after state consumption.
func (service *Service) CompleteCallbackWithLifecycle(ctx context.Context, input CallbackInput) (CallbackResult, CallbackLifecycle, error) {
	stateValue := strings.TrimSpace(input.State)
	if !validOpaqueValue(stateValue, 32, 512) {
		return CallbackResult{}, CallbackLifecycle{}, ErrInvalidState
	}

	now := service.clock.Now().UTC()
	state, err := service.states.Consume(ctx, sha256.Sum256([]byte(stateValue)), now)
	if err != nil || !state.ExpiresAt.After(now) {
		return CallbackResult{}, CallbackLifecycle{}, ErrInvalidState
	}
	lifecycle := CallbackLifecycle{TenantID: state.TenantID, ProviderCode: state.ProviderCode}
	browserBinding := strings.TrimSpace(input.BrowserBinding)
	browserBindingHash := sha256.Sum256([]byte(browserBinding))
	if !validOpaqueValue(browserBinding, 32, 512) || subtle.ConstantTimeCompare(browserBindingHash[:], state.BrowserBindingHash[:]) != 1 {
		return CallbackResult{}, lifecycle, ErrInvalidState
	}
	if strings.TrimSpace(input.ProviderError) != "" {
		return CallbackResult{}, lifecycle, ErrAuthorizationDenied
	}

	code := strings.TrimSpace(input.AuthorizationCode)
	if code == "" || len(code) > maxAuthorizationCodeLength || strings.ContainsAny(code, "\r\n") {
		return CallbackResult{}, lifecycle, ErrInvalidRequest
	}

	provider, err := service.providers.ResolveProvider(ctx, state.TenantID, state.ProviderCode)
	if err != nil || !sameProvider(provider, state, service.config.AllowInsecureHTTP) {
		return CallbackResult{}, lifecycle, ErrProviderUnavailable
	}
	tokens, err := service.remote.ExchangeAuthorizationCode(ctx, domain.TokenExchange{
		TokenEndpoint: state.Discovery.TokenEndpoint, ClientID: provider.ClientID, ClientSecret: provider.ClientSecret,
		ClientAuthenticationMode: provider.ClientAuthenticationMode, AuthorizationCode: code,
		RedirectURI: provider.RedirectURI, PKCEVerifier: state.PKCEVerifier,
	})
	if err != nil || strings.TrimSpace(tokens.IDToken) == "" {
		return CallbackResult{}, lifecycle, ErrTokenValidation
	}
	keys, err := service.remote.FetchJWKSet(ctx, state.Discovery.JWKSURI)
	if err != nil {
		return CallbackResult{}, lifecycle, ErrTokenValidation
	}
	claims, err := validateIDToken(tokens.IDToken, keys, idTokenExpectation{
		Issuer: state.Issuer, ClientID: state.ClientID, Nonce: state.Nonce, AccessToken: tokens.AccessToken,
		Now: now, ClockSkew: service.config.ClockSkew,
	})
	if err != nil {
		return CallbackResult{}, lifecycle, ErrTokenValidation
	}

	account, err := service.accounts.ResolveAccount(ctx, state.TenantID, state.ProviderCode, claims.Subject)
	if err != nil || !validAccount(account, state.TenantID) {
		return CallbackResult{}, lifecycle, ErrAccountNotBound
	}
	lifecycle.ProviderID = account.ProviderID
	lifecycle.BindingID = account.BindingID
	lifecycle.UserID = account.UserID
	lifecycle.AccountID = account.AccountID

	session, err := service.sessions.IssueBrowserSession(ctx, domain.SessionIssue{
		TenantID: account.TenantID, UserID: account.UserID, AccountID: account.AccountID, ProviderCode: state.ProviderCode,
		ProviderID: account.ProviderID, BindingID: account.BindingID, AuthenticationMethod: "EXTERNAL_OIDC",
		IPAddress: normalizeIP(input.IPAddress), UserAgent: truncateUserAgent(input.UserAgent),
	})
	if err != nil || !validBrowserSessionOutcome(session, now) {
		return CallbackResult{}, lifecycle, ErrSessionIssue
	}
	return CallbackResult{Session: session, RedirectTo: state.ReturnTo}, lifecycle, nil
}

func validBrowserSessionOutcome(session domain.BrowserSession, now time.Time) bool {
	if session.MFARequired {
		return strings.TrimSpace(session.CookieValue) == "" && session.ExpiresAt.IsZero() &&
			strings.TrimSpace(session.PreAuthenticationCredential) != "" &&
			session.PreAuthenticationExpiresAt.After(now) && session.MFAMaxAttempts > 0
	}

	return strings.TrimSpace(session.CookieValue) != "" && session.ExpiresAt.After(now) &&
		strings.TrimSpace(session.PreAuthenticationCredential) == "" &&
		session.PreAuthenticationExpiresAt.IsZero() && session.MFAMaxAttempts == 0
}

func (service *Service) newProtocolSecrets() (string, string, string, error) {
	state, err := service.secrets.NewSecret()
	if err != nil || !validOpaqueValue(state, 32, 512) {
		return "", "", "", errors.New("invalid state")
	}
	nonce, err := service.secrets.NewSecret()
	if err != nil || !validOpaqueValue(nonce, 32, 512) {
		return "", "", "", errors.New("invalid nonce")
	}
	verifier, err := service.secrets.NewSecret()
	if err != nil || !validPKCEVerifier(verifier) {
		return "", "", "", errors.New("invalid PKCE verifier")
	}
	return state, nonce, verifier, nil
}

func authorizationURL(endpoint string, provider domain.Provider, state, nonce, verifier string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid authorization endpoint")
	}
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", provider.ClientID)
	query.Set("redirect_uri", provider.RedirectURI)
	query.Set("scope", strings.Join(normalizedScopes(provider.Scopes), " "))
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", pkceChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func sameProvider(provider domain.Provider, state domain.State, allowInsecureHTTP bool) bool {
	return validProvider(provider, state.TenantID, state.ProviderCode, allowInsecureHTTP) && provider.Issuer == state.Issuer && provider.ClientID == state.ClientID && provider.RedirectURI == state.RedirectURI
}

func validProvider(provider domain.Provider, tenantID, providerCode string, allowInsecureHTTP bool) bool {
	if strings.TrimSpace(provider.TenantID) != tenantID || strings.TrimSpace(provider.Code) != providerCode ||
		!validProtocolURL(provider.Issuer, allowInsecureHTTP, false) || !validProtocolURL(provider.RedirectURI, allowInsecureHTTP, true) || strings.TrimSpace(provider.ClientID) == "" ||
		!validClientAuthenticationMode(provider.ClientAuthenticationMode) {
		return false
	}
	if provider.ClientAuthenticationMode != domain.ClientAuthenticationNone && strings.TrimSpace(provider.ClientSecret) == "" {
		return false
	}
	return len(normalizedScopes(provider.Scopes)) > 0
}

func validDiscovery(discovery domain.Discovery, issuer string, allowInsecureHTTP bool) bool {
	return discovery.Issuer == issuer && validProtocolURL(discovery.AuthorizationEndpoint, allowInsecureHTTP, true) && validProtocolURL(discovery.TokenEndpoint, allowInsecureHTTP, true) && validProtocolURL(discovery.JWKSURI, allowInsecureHTTP, true)
}

func validAccount(account domain.LocalAccount, tenantID string) bool {
	return account.TenantID == tenantID && strings.TrimSpace(account.ProviderID) != "" && strings.TrimSpace(account.BindingID) != "" && strings.TrimSpace(account.UserID) != "" && strings.TrimSpace(account.AccountID) != ""
}

func validClientAuthenticationMode(value string) bool {
	return value == domain.ClientAuthenticationSecretBasic || value == domain.ClientAuthenticationSecretPost || value == domain.ClientAuthenticationNone
}

func normalizedScopes(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes)+1)
	result := make([]string, 0, len(scopes)+1)
	seen["openid"] = struct{}{}
	result = append(result, "openid")
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			continue
		}
		if _, exists := seen[scope]; !exists {
			seen[scope] = struct{}{}
			result = append(result, scope)
		}
	}
	return result
}

func normalizeReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func validProtocolURL(value string, allowInsecureHTTP, allowQuery bool) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.ContainsAny(parsed.Host, " \t\r\n") {
		return false
	}
	if parsed.Scheme != "https" && !(allowInsecureHTTP && parsed.Scheme == "http") {
		return false
	}
	return allowQuery || parsed.RawQuery == ""
}

func validOpaqueValue(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func validPKCEVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~", char)) {
			return false
		}
	}
	return true
}

func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func normalizeIP(ip net.IP) []byte {
	if ip == nil {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return append([]byte(nil), ipv6...)
	}
	return nil
}
func truncateUserAgent(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

type idTokenExpectation struct {
	Issuer, ClientID, Nonce, AccessToken string
	Now                                  time.Time
	ClockSkew                            time.Duration
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}
type jwtPayload struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	Nonce           string          `json:"nonce"`
	IssuedAt        json.Number     `json:"iat"`
	ExpiresAt       json.Number     `json:"exp"`
	NotBefore       json.Number     `json:"nbf"`
	AuthorizedParty string          `json:"azp"`
	AccessTokenHash string          `json:"at_hash"`
}

func validateIDToken(token string, keys domain.JWKSet, expected idTokenExpectation) (domain.IDTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return domain.IDTokenClaims{}, errors.New("invalid compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.KeyID == "" {
		return domain.IDTokenClaims{}, errors.New("invalid JWT header")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payloadBytes)))
	decoder.UseNumber()
	var payload jwtPayload
	if err := decoder.Decode(&payload); err != nil {
		return domain.IDTokenClaims{}, err
	}
	audiences, err := parseAudience(payload.Audience)
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	issuedAt, err := unixNumber(payload.IssuedAt)
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	expiresAt, err := unixNumber(payload.ExpiresAt)
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	var notBefore *time.Time
	if payload.NotBefore != "" {
		value, err := unixNumber(payload.NotBefore)
		if err != nil {
			return domain.IDTokenClaims{}, err
		}
		notBefore = &value
	}
	if payload.Issuer != expected.Issuer || strings.TrimSpace(payload.Subject) == "" || !contains(audiences, expected.ClientID) || subtle.ConstantTimeCompare([]byte(payload.Nonce), []byte(expected.Nonce)) != 1 {
		return domain.IDTokenClaims{}, errors.New("JWT claims do not match authorization request")
	}
	if len(audiences) > 1 && payload.AuthorizedParty != expected.ClientID {
		return domain.IDTokenClaims{}, errors.New("JWT azp does not identify client")
	}
	now := expected.Now.UTC()
	if !expiresAt.After(now.Add(-expected.ClockSkew)) || issuedAt.After(now.Add(expected.ClockSkew)) || (notBefore != nil && notBefore.After(now.Add(expected.ClockSkew))) {
		return domain.IDTokenClaims{}, errors.New("JWT timing is invalid")
	}
	if payload.AccessTokenHash != "" && !validAccessTokenHash(payload.AccessTokenHash, expected.AccessToken, header.Algorithm) {
		return domain.IDTokenClaims{}, errors.New("JWT access token hash is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	key, err := selectJWK(keys, header.KeyID, header.Algorithm)
	if err != nil {
		return domain.IDTokenClaims{}, err
	}
	if err := verifyJWTSignature(header.Algorithm, key, []byte(parts[0]+"."+parts[1]), signature); err != nil {
		return domain.IDTokenClaims{}, err
	}
	return domain.IDTokenClaims{Issuer: payload.Issuer, Subject: payload.Subject, Audience: audiences, Nonce: payload.Nonce, IssuedAt: issuedAt, ExpiresAt: expiresAt, NotBefore: notBefore, AuthorizedParty: payload.AuthorizedParty, AccessTokenHash: payload.AccessTokenHash}, nil
}

func parseAudience(raw json.RawMessage) ([]string, error) {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) == 0 {
		return nil, errors.New("invalid JWT audience")
	}
	for _, value := range many {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("invalid JWT audience")
		}
	}
	return many, nil
}
func unixNumber(value json.Number) (time.Time, error) {
	seconds, err := value.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("invalid JWT time claim")
	}
	return time.Unix(seconds, 0).UTC(), nil
}
func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func selectJWK(keys domain.JWKSet, keyID, algorithm string) (domain.JWK, error) {
	for _, key := range keys.Keys {
		if key.KeyID == keyID && (key.Algorithm == "" || key.Algorithm == algorithm) && (key.Use == "" || key.Use == "sig") {
			return key, nil
		}
	}
	return domain.JWK{}, errors.New("JWT signing key is unavailable")
}

func verifyJWTSignature(algorithm string, key domain.JWK, signingInput, signature []byte) error {
	switch algorithm {
	case "RS256":
		if key.KeyType != "RSA" {
			return errors.New("JWT key type does not match algorithm")
		}
		publicKey, err := rsaPublicKey(key)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(signingInput)
		if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature) != nil {
			return errors.New("JWT signature verification failed")
		}
	case "ES256":
		if key.KeyType != "EC" || key.Curve != "P-256" {
			return errors.New("JWT key type does not match algorithm")
		}
		publicKey, err := ecdsaPublicKey(key)
		if err != nil {
			return err
		}
		if len(signature) != 64 {
			return errors.New("invalid ECDSA signature")
		}
		digest := sha256.Sum256(signingInput)
		if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
			return errors.New("JWT signature verification failed")
		}
	case "EdDSA":
		if key.KeyType != "OKP" || key.Curve != "Ed25519" {
			return errors.New("JWT key type does not match algorithm")
		}
		raw, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return errors.New("invalid Ed25519 JWK")
		}
		if !ed25519.Verify(ed25519.PublicKey(raw), signingInput, signature) {
			return errors.New("JWT signature verification failed")
		}
	default:
		return errors.New("unsupported JWT signature algorithm")
	}
	return nil
}

func rsaPublicKey(key domain.JWK) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(key.Modulus)
	if err != nil || len(modulus) < 256 {
		return nil, errors.New("invalid RSA JWK modulus")
	}
	exponent, err := base64.RawURLEncoding.DecodeString(key.Exponent)
	if err != nil || len(exponent) == 0 || len(exponent) > 4 {
		return nil, errors.New("invalid RSA JWK exponent")
	}
	value := 0
	for _, part := range exponent {
		value = value<<8 | int(part)
	}
	if value < 3 || value%2 == 0 {
		return nil, errors.New("invalid RSA JWK exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: value}, nil
}
func ecdsaPublicKey(key domain.JWK) (*ecdsa.PublicKey, error) {
	x, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return nil, errors.New("invalid ECDSA JWK x")
	}
	y, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return nil, errors.New("invalid ECDSA JWK y")
	}
	curve := elliptic.P256()
	pointX, pointY := new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)
	if !curve.IsOnCurve(pointX, pointY) {
		return nil, errors.New("ECDSA JWK point is invalid")
	}
	return &ecdsa.PublicKey{Curve: curve, X: pointX, Y: pointY}, nil
}
func validAccessTokenHash(expectedHash, accessToken, algorithm string) bool {
	if accessToken == "" {
		return false
	}
	if algorithm != "RS256" && algorithm != "ES256" && algorithm != "EdDSA" {
		return false
	}
	digest := sha256.Sum256([]byte(accessToken))
	value := base64.RawURLEncoding.EncodeToString(digest[:len(digest)/2])
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(value)) == 1
}
