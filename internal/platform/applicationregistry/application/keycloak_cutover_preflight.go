package application

import (
	"net/url"
	"strings"
)

// KeycloakCutoverTransport 是 Keycloak 运行时切换中面向浏览器的传输参数。
// 刻意不含客户端凭据：部署 Agent 写受保护运行时文件时会独立推导相同取值，
// 浏览器面与凭据面分离可缩小凭据暴露面。
type KeycloakCutoverTransport struct {
	PublicURL    string
	RedirectURI  string
	CookieSecure bool
}

// ValidateKeycloakCutoverTransport 把三项浏览器传输参数作为整体校验，
// 派生 PublicURL 与 /auth/callback 回调地址。RequireHTTPS 为 false 时仍允许
// HTTP 部署，以兼容既有可信内网环境；策略一旦启用，HTTPS 与安全会话 Cookie
// 即成为环境切到 Keycloak Issuer 的硬性前置条件。
func ValidateKeycloakCutoverTransport(publicBaseURL, pathPrefix string, requireHTTPS bool) (KeycloakCutoverTransport, error) {
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	path := strings.TrimRight(strings.TrimSpace(pathPrefix), "/")
	if base == "" || path == "" || !strings.HasPrefix(path, "/") {
		return KeycloakCutoverTransport{}, ErrValidation
	}
	parsed, err := url.ParseRequestURI(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return KeycloakCutoverTransport{}, ErrValidation
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return KeycloakCutoverTransport{}, ErrValidation
	}
	if requireHTTPS && !strings.EqualFold(parsed.Scheme, "https") {
		return KeycloakCutoverTransport{}, ErrValidation
	}

	publicURL := base + path + "/"
	redirectURI := base + path + "/auth/callback"
	redirect, err := url.ParseRequestURI(redirectURI)
	if err != nil || redirect.Scheme != parsed.Scheme || redirect.Host != parsed.Host || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" {
		return KeycloakCutoverTransport{}, ErrValidation
	}
	return KeycloakCutoverTransport{
		PublicURL: publicURL, RedirectURI: redirectURI,
		CookieSecure: strings.EqualFold(parsed.Scheme, "https"),
	}, nil
}
