package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

const (
	maxDiscoveryBytes = 256 << 10
	maxJWKSetBytes    = 512 << 10
	maxTokenBytes     = 128 << 10
)

// HTTPClientConfig controls outbound OIDC network behavior. HTTPS is mandatory by default. When a
// non-production test provider requires HTTP, AllowInsecureHTTP must be explicitly enabled and the
// target host must still be allow-listed.
type HTTPClientConfig struct {
	Timeout           time.Duration
	AllowInsecureHTTP bool
	AllowedHosts      []string
}

// HTTPClient executes discovery, token exchange and JWKS retrieval without following redirects.
// It contains no logging so codes, tokens, subjects and client credentials cannot leak through it.
type HTTPClient struct {
	client            *http.Client
	allowInsecureHTTP bool
	allowedHosts      map[string]struct{}
}

// NewHTTPClient validates outbound URL policy and creates a bounded client.
func NewHTTPClient(config HTTPClientConfig) (*HTTPClient, error) {
	if config.Timeout <= 0 || config.Timeout > time.Minute {
		return nil, errors.New("external OIDC HTTP timeout must be between zero and one minute")
	}
	allowedHosts := make(map[string]struct{}, len(config.AllowedHosts))
	for _, host := range config.AllowedHosts {
		normalized, err := normalizedHost(host)
		if err != nil {
			return nil, errors.New("external OIDC allowed host is invalid")
		}
		allowedHosts[normalized] = struct{}{}
	}
	if len(allowedHosts) == 0 {
		return nil, errors.New("external OIDC allowed hosts must not be empty")
	}
	return &HTTPClient{
		client: &http.Client{Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		allowInsecureHTTP: config.AllowInsecureHTTP,
		allowedHosts:      allowedHosts,
	}, nil
}

// Discover loads and validates the OpenID Provider Configuration Document for the exact issuer.
func (client *HTTPClient) Discover(ctx context.Context, issuer string) (domain.Discovery, error) {
	issuerURL, err := client.validateURL(issuer)
	if err != nil || issuerURL.RawQuery != "" {
		return domain.Discovery{}, errors.New("invalid OIDC issuer URL")
	}
	issuerURL.Path = strings.TrimRight(issuerURL.Path, "/") + "/.well-known/openid-configuration"
	issuerURL.RawQuery = ""
	issuerURL.Fragment = ""
	var payload struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKSURI               string `json:"jwks_uri"`
	}
	if err := client.getJSON(ctx, issuerURL.String(), maxDiscoveryBytes, &payload); err != nil {
		return domain.Discovery{}, errors.New("OIDC discovery request failed")
	}
	if payload.Issuer != issuer || client.validateEndpoint(payload.AuthorizationEndpoint) != nil || client.validateEndpoint(payload.TokenEndpoint) != nil || client.validateEndpoint(payload.JWKSURI) != nil {
		return domain.Discovery{}, errors.New("OIDC discovery document is invalid")
	}
	return domain.Discovery{Issuer: payload.Issuer, AuthorizationEndpoint: payload.AuthorizationEndpoint, TokenEndpoint: payload.TokenEndpoint, JWKSURI: payload.JWKSURI}, nil
}

// ExchangeAuthorizationCode sends a private form-encoded authorization-code request. It never
// returns the upstream response body in an error.
func (client *HTTPClient) ExchangeAuthorizationCode(ctx context.Context, exchange domain.TokenExchange) (domain.TokenResponse, error) {
	if err := client.validateEndpoint(exchange.TokenEndpoint); err != nil || !validClientAuthenticationMode(exchange.ClientAuthenticationMode) || strings.TrimSpace(exchange.ClientID) == "" || strings.TrimSpace(exchange.AuthorizationCode) == "" || strings.TrimSpace(exchange.RedirectURI) == "" || strings.TrimSpace(exchange.PKCEVerifier) == "" {
		return domain.TokenResponse{}, errors.New("invalid OIDC token exchange")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", exchange.AuthorizationCode)
	form.Set("redirect_uri", exchange.RedirectURI)
	form.Set("client_id", exchange.ClientID)
	form.Set("code_verifier", exchange.PKCEVerifier)
	if exchange.ClientAuthenticationMode == domain.ClientAuthenticationSecretPost {
		form.Set("client_secret", exchange.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, exchange.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return domain.TokenResponse{}, errors.New("create OIDC token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if exchange.ClientAuthenticationMode == domain.ClientAuthenticationSecretBasic {
		request.SetBasicAuth(exchange.ClientID, exchange.ClientSecret)
	}
	response, err := client.client.Do(request)
	if err != nil {
		return domain.TokenResponse{}, errors.New("OIDC token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		io.Copy(io.Discard, io.LimitReader(response.Body, maxTokenBytes))
		return domain.TokenResponse{}, errors.New("OIDC token endpoint rejected authorization code")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenBytes+1))
	if err != nil || len(body) > maxTokenBytes {
		return domain.TokenResponse{}, errors.New("OIDC token response is invalid")
	}
	var payload struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.IDToken) == "" {
		return domain.TokenResponse{}, errors.New("OIDC token response is invalid")
	}
	return domain.TokenResponse{IDToken: payload.IDToken, AccessToken: payload.AccessToken, TokenType: payload.TokenType, ExpiresIn: payload.ExpiresIn}, nil
}

// FetchJWKSet retrieves an issuer's public verification keys.
func (client *HTTPClient) FetchJWKSet(ctx context.Context, jwksURI string) (domain.JWKSet, error) {
	if err := client.validateEndpoint(jwksURI); err != nil {
		return domain.JWKSet{}, errors.New("invalid OIDC JWKS URL")
	}
	var keys domain.JWKSet
	if err := client.getJSON(ctx, jwksURI, maxJWKSetBytes, &keys); err != nil || len(keys.Keys) == 0 {
		return domain.JWKSet{}, errors.New("OIDC JWKS response is invalid")
	}
	return keys, nil
}

func (client *HTTPClient) getJSON(ctx context.Context, endpoint string, limit int64, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		io.Copy(io.Discard, io.LimitReader(response.Body, limit))
		return errors.New("non-success OIDC response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return errors.New("OIDC response exceeds configured limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	return decoder.Decode(target)
}

func (client *HTTPClient) validateEndpoint(value string) error {
	_, err := client.validateURL(value)
	return err
}

func (client *HTTPClient) validateURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme == "" {
		return nil, errors.New("OIDC URL is malformed")
	}
	if parsed.Scheme != "https" && !(client.allowInsecureHTTP && parsed.Scheme == "http") {
		return nil, errors.New("OIDC URL must use HTTPS")
	}
	host, err := normalizedHost(parsed.Host)
	if err != nil {
		return nil, err
	}
	if _, allowed := client.allowedHosts[host]; !allowed {
		return nil, errors.New("OIDC URL host is not allow-listed")
	}
	return parsed, nil
}

func normalizedHost(value string) (string, error) {
	parsed, err := url.Parse("https://" + strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid host")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("invalid host")
	}
	port := parsed.Port()
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

func validClientAuthenticationMode(value string) bool {
	return value == domain.ClientAuthenticationSecretBasic || value == domain.ClientAuthenticationSecretPost || value == domain.ClientAuthenticationNone
}

// AllowedHosts returns a sorted copy for startup diagnostics that never includes a credential.
func (client *HTTPClient) AllowedHosts() []string {
	result := make([]string, 0, len(client.allowedHosts))
	for host := range client.allowedHosts {
		result = append(result, host)
	}
	sort.Strings(result)
	return result
}
