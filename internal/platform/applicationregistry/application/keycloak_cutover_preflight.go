package application

import (
	"net/url"
	"strings"
)

// KeycloakCutoverTransport is the browser-facing part of a Keycloak runtime
// switch.  It intentionally contains no Client credential: the deployment
// Agent derives the same values while writing its protected runtime file.
type KeycloakCutoverTransport struct {
	PublicURL    string
	RedirectURI  string
	CookieSecure bool
}

// ValidateKeycloakCutoverTransport keeps the three browser transport settings
// together.  HTTP remains a supported deployment mode while RequireHTTPS is
// false, which preserves current trusted-network deployments.  Enabling the
// policy later makes HTTPS and a secure session cookie mandatory before an
// environment can move to the Keycloak issuer.
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
