package bootstrap

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// keycloakAuthorizationVerifier 把范围受限的 broker 验证器适配到
// authorization-context 端点。它不会使 Keycloak token 对平台管理 API 或平台
// OAuth token 端点变为有效。
type keycloakAuthorizationVerifier struct {
	verifier *middleware.KeycloakBrokerJWTVerifier
}

// Verify 验签 Keycloak 签发的访问 token 并提取 authorization-context 所需的紧凑声明。
func (v keycloakAuthorizationVerifier) Verify(ctx context.Context, raw string) (oidchttp.ExternalAuthorizationTokenClaims, error) {
	claims, err := v.verifier.VerifyAuthorizationAccessToken(ctx, raw)
	if err != nil {
		return oidchttp.ExternalAuthorizationTokenClaims{}, err
	}
	return oidchttp.ExternalAuthorizationTokenClaims{
		Subject: claims.Subject, IdentityID: claims.IdentityID, TenantID: claims.TenantID, SessionID: claims.SessionID,
		AuthorizedParty: claims.AuthorizedParty, Audience: append([]string(nil), claims.Audience...), TokenUse: claims.TokenUse,
	}, nil
}

// keycloakJWKSBackchannel 在 token 校验中保留公开 issuer 的同时，
// 允许 API 容器经 Compose 内网拉取签名密钥（JWKS）。
type keycloakJWKSBackchannel struct {
	base             http.RoundTripper
	public, internal *url.URL
}

// RoundTrip 仅对匹配公开 issuer 的请求改写到 Compose 内网地址，其余请求原样转发，
// 保证 token 校验仍以公开 issuer 为准。
func (t keycloakJWKSBackchannel) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.public.Scheme || request.URL.Host != t.public.Host {
		return t.base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = t.internal.Scheme, t.internal.Host
	return t.base.RoundTrip(clone)
}

// newKeycloakAuthorizationVerifier 装配有界网络访问的上游签名验证器。
// 参数：cfgIssuer为受配置管理的预期发行者。
//
//	cfgAdminURL为受控Keycloak管理入口。
//	realm为既有受控Realm配置。
//
// 返回值：装配后的应用或认证实例；配置与依赖装配失败时返回错误。
func newKeycloakAuthorizationVerifier(cfgIssuer, cfgAdminURL, realm string) (*keycloakAuthorizationVerifier, error) {
	publicIssuer := strings.TrimRight(cfgIssuer, "/") + "/realms/" + strings.TrimSpace(realm)
	internalIssuer := strings.TrimRight(cfgAdminURL, "/") + "/realms/" + strings.TrimSpace(realm)
	public, err := url.Parse(publicIssuer)
	if err != nil {
		return nil, err
	}
	internal, err := url.Parse(internalIssuer)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: keycloakJWKSBackchannel{base: http.DefaultTransport, public: public, internal: internal}}
	verifier, err := middleware.NewKeycloakBrokerJWTVerifier(publicIssuer, client)
	if err != nil {
		return nil, err
	}
	return &keycloakAuthorizationVerifier{verifier: verifier}, nil
}

var _ oidchttp.ExternalAuthorizationTokenVerifier = (*keycloakAuthorizationVerifier)(nil)
