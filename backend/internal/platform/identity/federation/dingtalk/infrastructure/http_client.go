package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/domain"
)

const (
	defaultDingTalkHTTPTimeout = 10 * time.Second
	maxDingTalkResponseBytes   = 64 << 10
)

// HTTPClient calls only the fixed official DingTalk OAuth and contact endpoints. It deliberately
// returns no response body in errors because upstream error payloads may contain sensitive values.
type HTTPClient struct {
	client *http.Client
}

var _ application.DingTalkRemote = (*HTTPClient)(nil)

// NewHTTPClient constructs the protocol client. A caller may inject a transport for tests; all
// outgoing URLs remain fixed HTTPS DingTalk hosts regardless of the supplied client. Redirects are
// rejected so an authorization code, client secret, or access token can never be forwarded to a
// different origin by an upstream redirect response.
func NewHTTPClient(client *http.Client) (*HTTPClient, error) {
	if client == nil {
		client = &http.Client{Timeout: defaultDingTalkHTTPTimeout}
	}
	if client.Timeout <= 0 {
		return nil, errors.New("dingtalk HTTP client timeout must be positive")
	}
	securedClient := *client
	securedClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &HTTPClient{client: &securedClient}, nil
}

// ResolveUser exchanges an authorization code and uses the returned access token only for the
// immediately following user-info request. Tokens and response bodies never escape this function.
func (client *HTTPClient) ResolveUser(ctx context.Context, provider domain.Provider, authorizationCode string) (domain.UserProfile, error) {
	if strings.TrimSpace(provider.AppKey) == "" || strings.TrimSpace(provider.AppSecret) == "" || strings.TrimSpace(authorizationCode) == "" {
		return domain.UserProfile{}, application.ErrProtocolValidation
	}
	accessToken, err := client.exchangeAuthorizationCode(ctx, provider.AppKey, provider.AppSecret, authorizationCode)
	if err != nil {
		return domain.UserProfile{}, application.ErrProtocolValidation
	}
	unionID, err := client.fetchUnionID(ctx, accessToken)
	if err != nil || strings.TrimSpace(unionID) == "" {
		return domain.UserProfile{}, application.ErrProtocolValidation
	}
	return domain.UserProfile{UnionID: unionID}, nil
}

func (client *HTTPClient) exchangeAuthorizationCode(ctx context.Context, appKey, appSecret, code string) (string, error) {
	payload, err := json.Marshal(struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		Code         string `json:"code"`
		GrantType    string `json:"grantType"`
	}{ClientID: appKey, ClientSecret: appSecret, Code: code, GrantType: "authorization_code"})
	if err != nil {
		return "", application.ErrProtocolValidation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, domain.TokenEndpoint, bytes.NewReader(payload))
	if err != nil || !validOfficialDingTalkURL(request.URL, "api.dingtalk.com", "/v1.0/oauth2/userAccessToken") {
		return "", application.ErrProtocolValidation
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return "", application.ErrProtocolValidation
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", application.ErrProtocolValidation
	}
	var body struct {
		AccessToken string `json:"accessToken"`
	}
	if err := decodeBoundedJSON(response.Body, &body); err != nil || strings.TrimSpace(body.AccessToken) == "" {
		return "", application.ErrProtocolValidation
	}
	return body.AccessToken, nil
}

func (client *HTTPClient) fetchUnionID(ctx context.Context, accessToken string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, domain.UserInfoEndpoint, nil)
	if err != nil || !validOfficialDingTalkURL(request.URL, "api.dingtalk.com", "/v1.0/contact/users/me") {
		return "", application.ErrProtocolValidation
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-acs-dingtalk-access-token", accessToken)
	response, err := client.client.Do(request)
	if err != nil {
		return "", application.ErrProtocolValidation
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", application.ErrProtocolValidation
	}
	var body struct {
		UnionID string `json:"unionId"`
	}
	if err := decodeBoundedJSON(response.Body, &body); err != nil {
		return "", application.ErrProtocolValidation
	}
	return strings.TrimSpace(body.UnionID), nil
}

func decodeBoundedJSON(body io.Reader, destination any) error {
	content, err := io.ReadAll(io.LimitReader(body, maxDingTalkResponseBytes+1))
	if err != nil {
		return err
	}
	if len(content) > maxDingTalkResponseBytes {
		return errors.New("DingTalk response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected trailing DingTalk response data")
	}
	return nil
}

func validOfficialDingTalkURL(parsed *url.URL, host, path string) bool {
	return parsed != nil && parsed.Scheme == "https" && parsed.Host == host && parsed.User == nil && parsed.Path == path && parsed.RawQuery == ""
}
