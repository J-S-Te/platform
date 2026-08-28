// Package oidchttp adapts the OIDC/OAuth application service to protocol HTTP endpoints.
package oidchttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

const (
	maxAuthorizeRequestBytes = 16 << 10
	maxTokenRequestBytes     = 64 << 10
)

// Service is the OIDC application boundary used by this HTTP adapter. Keeping it as an interface
// makes HTTP tests independent from persistence and lets bootstrap inject application.Service.
type Service interface {
	Authorize(context.Context, application.AuthorizationInput) (application.AuthorizationResult, error)
	ResolveRequestObject(context.Context, application.RequestObjectAuthorizationInput) (application.AuthorizationInput, error)
	PushAuthorizationRequest(context.Context, application.PushAuthorizationRequestInput) (application.PushAuthorizationRequestResult, error)
	ConsumePushedAuthorizationRequest(context.Context, application.ConsumePushedAuthorizationRequestInput) (application.AuthorizationInput, error)
	DecideConsent(context.Context, application.ConsentInput) (application.ConsentDecision, error)
	GrantConsent(context.Context, application.ConsentInput) error
	RevokeConsent(context.Context, string, string, string) error
	ExchangeAuthorizationCode(context.Context, application.AuthorizationCodeExchangeInput) (application.TokenResult, error)
	Refresh(context.Context, application.RefreshTokenInput) (application.TokenResult, error)
	Revoke(context.Context, application.RevokeTokenInput) error
	IsAccessTokenRevoked(context.Context, string, string) (bool, error)
	ResolveUserInfo(context.Context, application.UserInfoInput) (application.UserInfo, error)
}

// LegacyClientCredentialsIssuer preserves the existing client_credentials behavior while the
// platform moves all public OAuth endpoints behind this handler.
type LegacyClientCredentialsIssuer interface {
	IssueClientCredentials(context.Context, string, string, []string) (applicationregistryapplication.TokenResult, error)
}

// BrowserSessionAuthenticator verifies the platform's signed browser-session cookie. The identity
// application service already exposes a method with this shape, so bootstrap may inject it directly.
type BrowserSessionAuthenticator interface {
	Authenticate(context.Context, string) (authctx.Principal, error)
}

// BrowserSessionLogout revokes an already authenticated platform session. It is deliberately
// separate from BrowserSessionAuthenticator because deployments may initially publish OIDC before
// wiring RP-initiated logout to the identity service.
type BrowserSessionLogout interface {
	Logout(context.Context, authctx.Principal) error
}

// JWTManager represents the OIDC signing-key operations needed at the protocol edge.
type JWTManager interface {
	Issuer() string
	JWKS() security.OIDCJWKS
	VerifyAccessToken(token, expectedAudience string, now time.Time) (security.OIDCTokenClaims, error)
	VerifyIDToken(token, expectedAudience string, now time.Time) (security.OIDCTokenClaims, error)
}

// PostLogoutRedirectValidator confirms a URI against the dedicated post-logout registration set.
// It must not reuse ordinary OAuth redirect URIs.
type PostLogoutRedirectValidator interface {
	IsRegisteredPostLogoutRedirectURI(context.Context, string, string) (bool, error)
}

// AccessTokenSubject contains the tenant-scoped durable identifiers omitted from the compact
// public JWT. A resolver must bind the signed client/session/user identifiers to active storage.
type AccessTokenSubject struct {
	TenantID      string
	OAuthClientID string
	LoginIP       string
}

// AccessTokenSubjectResolver resolves the durable OIDC client and tenant IDs needed by
// application.ResolveUserInfo and revocation checks. UserInfo fails closed when it is absent.
type AccessTokenSubjectResolver interface {
	ResolveAccessTokenSubject(context.Context, string, string, string) (AccessTokenSubject, error)
}

// ExternalAuthorizationTokenVerifier is the narrow trust boundary for an upstream
// identity provider such as Keycloak. It is used only by authorization-context;
// normal platform OAuth endpoints continue to accept platform-issued access tokens.
type ExternalAuthorizationTokenClaims struct {
	Subject         string
	IdentityID      string
	TenantID        string
	SessionID       string
	AuthorizedParty string
	Audience        []string
	TokenUse        string
}

type ExternalAuthorizationTokenVerifier interface {
	Verify(context.Context, string) (ExternalAuthorizationTokenClaims, error)
}

// PersonnelDirectoryEntry is the minimal tenant personnel projection exposed to profile-scoped
// subsystem sessions. Login names and employee numbers are deliberately excluded.
type PersonnelDirectoryEntry struct {
	UserID      string   `json:"user_id"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
}

type PersonnelDirectoryResolver interface {
	ListActivePersonnel(context.Context, string, string) ([]PersonnelDirectoryEntry, error)
}

// Clock makes JWT verification deterministic in unit tests.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Config lists all bootstrap-owned dependencies for the OIDC HTTP edge. SessionAuthenticator,
// SessionLogout, LegacyClientCredentialsIssuer, and PostLogoutRedirectValidator are intentionally
// optional: endpoints that depend on an absent adapter fail closed rather than accepting input.
type Config struct {
	Service                        Service
	JWTManager                     JWTManager
	LegacyClientCredentialsIssuer  LegacyClientCredentialsIssuer
	SessionAuthenticator           BrowserSessionAuthenticator
	SessionLogout                  BrowserSessionLogout
	PostLogoutRedirectValidator    PostLogoutRedirectValidator
	AccessTokenSubjectResolver     AccessTokenSubjectResolver
	ExternalAuthorizationVerifier  ExternalAuthorizationTokenVerifier
	AuthorizationResolver          tokenissuer.AuthorizationResolver
	AuthorizationContextResolver   tokenissuer.AuthorizationContextResolver
	PersonnelDirectoryResolver     PersonnelDirectoryResolver
	AllowLegacyPlatformAccessToken bool
	// CustomerBindingResolver 在 EmitCustomerRef 打开时向 authorization-context 响应
	// 追加 customer_ref 声明；实现由外部客户绑定模块注入。
	CustomerBindingResolver CustomerBindingResolver
	EmitCustomerRef         bool
	SessionCookieName       string
	SessionCookieSecure     bool
	SessionCookieSameSite   http.SameSite
	Clock                   Clock
	Logger                  *slog.Logger
}

// Handler contains only transport policy. Durable protocol state remains in application.Service.
type Handler struct {
	service                        Service
	jwtManager                     JWTManager
	legacyIssuer                   LegacyClientCredentialsIssuer
	sessionAuth                    BrowserSessionAuthenticator
	sessionLogout                  BrowserSessionLogout
	logoutRedirects                PostLogoutRedirectValidator
	accessTokenSubjects            AccessTokenSubjectResolver
	externalAuthorizationVerifier  ExternalAuthorizationTokenVerifier
	authorizationResolver          tokenissuer.AuthorizationResolver
	authorizationContextResolver   tokenissuer.AuthorizationContextResolver
	personnelDirectory             PersonnelDirectoryResolver
	allowLegacyPlatformAccessToken bool
	customerBindingResolver        CustomerBindingResolver
	emitCustomerRef                bool
	brokerSelfHealer               BrokerDriftSelfHealer
	cookie                         cookieConfig
	clock                          Clock
	logger                         *slog.Logger
}

// BrokerDriftSelfHealer repairs a platform OAuth broker client whose secret stored in
// Keycloak no longer matches the platform's active credential. It is invoked when the
// token endpoint rejects a broker client's authentication, which is the earliest point
// the platform can observe Keycloak using a stale secret. Implementations must be safe
// for concurrent use and must rate-limit repairs to avoid a failure storm.
type BrokerDriftSelfHealer interface {
	HealBrokerSecretDrift(ctx context.Context, clientID string) error
}

// SetBrokerDriftSelfHealer installs the broker secret drift self-healer. A nil value keeps
// the endpoint fail-closed on invalid broker credentials without attempting a repair.
func (h *Handler) SetBrokerDriftSelfHealer(healer BrokerDriftSelfHealer) {
	h.brokerSelfHealer = healer
}

// CustomerBindingResolver resolves the CRM customer reference bound to a platform
// identity inside one application. Any error means the claim is omitted from the
// authorization-context response; consumers fail closed on its absence.
type CustomerBindingResolver interface {
	ResolveCustomerBinding(ctx context.Context, tenantID, platformUserID, applicationCode string) (string, error)
}

type cookieConfig struct {
	name     string
	secure   bool
	sameSite http.SameSite
}

// NewHandler validates required protocol dependencies. A concrete *security.OIDCJWTManager and
// *oidcapplication.Service satisfy the interfaces in Config without an adapter.
func NewHandler(config Config) (*Handler, error) {
	if config.Service == nil || config.JWTManager == nil {
		return nil, errors.New("OIDC HTTP handler service and JWT manager must not be nil")
	}
	if strings.TrimSpace(config.JWTManager.Issuer()) == "" {
		return nil, errors.New("OIDC HTTP handler JWT issuer must not be empty")
	}
	if strings.TrimSpace(config.SessionCookieName) == "" {
		return nil, errors.New("OIDC HTTP handler session cookie name must not be empty")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Handler{
		service: config.Service, jwtManager: config.JWTManager, legacyIssuer: config.LegacyClientCredentialsIssuer,
		sessionAuth: config.SessionAuthenticator, sessionLogout: config.SessionLogout,
		logoutRedirects: config.PostLogoutRedirectValidator, accessTokenSubjects: config.AccessTokenSubjectResolver,
		externalAuthorizationVerifier: config.ExternalAuthorizationVerifier,
		authorizationResolver:         config.AuthorizationResolver, authorizationContextResolver: config.AuthorizationContextResolver,
		personnelDirectory:             config.PersonnelDirectoryResolver,
		allowLegacyPlatformAccessToken: config.AllowLegacyPlatformAccessToken,
		customerBindingResolver:        config.CustomerBindingResolver,
		emitCustomerRef:                config.EmitCustomerRef && config.CustomerBindingResolver != nil,
		cookie:                         cookieConfig{name: config.SessionCookieName, secure: config.SessionCookieSecure, sameSite: config.SessionCookieSameSite},
		clock:                          config.Clock, logger: config.Logger,
	}, nil
}
