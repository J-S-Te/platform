package http

// 本适配器只持有应用接入所需的 Keycloak Admin API 小型可审计子集。
// 刻意不把 Admin API 代理给浏览器，也绝不返回 access token 或客户端 Secret。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const keycloakAdminErrorSummaryLimit = 1024

var (
	keycloakAdminSensitiveValue = regexp.MustCompile(`(?i)("?(?:client[_-]?secret|clientsecret|password|access[_-]?token|refresh[_-]?token|token|authorization|credential|api[_-]?key)"?\s*[:=]\s*)("[^"]*"|[^\s,}&]+)`)
	keycloakAdminBearerToken    = regexp.MustCompile(`(?i)(\bauthorization\s*[:=]\s*bearer\s+)[A-Za-z0-9._~+/=-]+`)
	keycloakAdminCookieValue    = regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie)\s*[:=]\s*)("[^"]*"|[^\s,}&;]+(?:\s*;\s*[^\s,}&;]+)*)`)
	keycloakAdminJWT            = regexp.MustCompile(`\b[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

type keycloakControlPlane struct {
	adminURL, realm, username, password, adminClientID, adminClientSecret, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string
	httpClient                                                                                                                                     *stdhttp.Client
}

// KeycloakControlPlaneCredentials 显式提供控制面调用 Keycloak Admin API 所用凭据。
// 提供完整服务账号对时优先使用并以 client_credentials 兑换；
// 用户名/口令仍为既有部署保留，仅在未配置服务账号时使用。
//
// 凭据是进程内输入；适配器绝不将其返回给任何调用方。
type KeycloakControlPlaneCredentials struct {
	ServiceAccountClientID     string
	ServiceAccountClientSecret string
	Username                   string
	Password                   string
}

type keycloakClientResult struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"-"`
}

// keycloakIdentityClaimMappings 是基础平台身份/授权模型到受代理 Keycloak 用户的
// 显式单向投影。Keycloak 可以把这些值转发给依赖方，但绝不允许成为人员、
// 组织或授权数据的事实来源。
var keycloakIdentityClaimMappings = []keycloakIdentityClaimMapping{
	// identity_id 是不可变的平台主体。发布为 OIDC subject，
	// 使每个依赖方都使用同一稳定身份键。
	{Name: "identity_id", ClaimName: "sub"},
	{Name: "tenant_id"},
	{Name: "person_id"},
}

type keycloakIdentityClaimMapping struct {
	Name        string
	ClaimName   string
	MultiValued bool
}

type keycloakManagedProtocolMapper struct {
	Name           string
	ProtocolMapper string
	Config         map[string]string
}

func newKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string) *keycloakControlPlane {
	return newKeycloakControlPlaneWithCredentials(adminURL, realm, KeycloakControlPlaneCredentials{Username: username, Password: password}, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel)
}

func newKeycloakControlPlaneWithCredentials(adminURL, realm string, credentials KeycloakControlPlaneCredentials, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string) *keycloakControlPlane {
	return &keycloakControlPlane{adminURL: strings.TrimRight(adminURL, "/"), realm: realm, username: credentials.Username, password: credentials.Password, adminClientID: credentials.ServiceAccountClientID, adminClientSecret: credentials.ServiceAccountClientSecret, brokerClientID: brokerClientID, brokerClientSecret: brokerClientSecret, platformIssuer: strings.TrimRight(platformIssuer, "/"), platformBackchannel: strings.TrimRight(platformBackchannel, "/"), httpClient: &stdhttp.Client{Timeout: 12 * time.Second}}
}

// NewKeycloakControlPlane 刻意返回不透明的适配器：仅允许 bootstrap 包
// 从进程密钥构造它，其余代码不得自行拼装控制面客户端。
func NewKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string) *keycloakControlPlane {
	return newKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel)
}

// NewKeycloakControlPlaneWithCredentials 以显式注入的 Admin API 凭据构造控制面。
// 完整服务账号对走 client_credentials；否则按向后兼容使用用户名/口令对。
func NewKeycloakControlPlaneWithCredentials(adminURL, realm string, credentials KeycloakControlPlaneCredentials, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string) *keycloakControlPlane {
	return newKeycloakControlPlaneWithCredentials(adminURL, realm, credentials, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel)
}

func (control *keycloakControlPlane) token(ctx context.Context) (string, error) {
	form := url.Values{}
	if strings.TrimSpace(control.adminClientID) != "" && strings.TrimSpace(control.adminClientSecret) != "" {
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", control.adminClientID)
		form.Set("client_secret", control.adminClientSecret)
	} else {
		form.Set("grant_type", "password")
		form.Set("client_id", "admin-cli")
		form.Set("username", control.username)
		form.Set("password", control.password)
	}
	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, control.adminURL+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := control.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("keycloak admin authentication", response, stdhttp.StatusOK); err != nil {
		return "", err
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("keycloak admin authentication returned no token")
	}
	return payload.AccessToken, nil
}

func (control *keycloakControlPlane) request(ctx context.Context, token, method, path string, body any) (*stdhttp.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := stdhttp.NewRequestWithContext(ctx, method, control.adminURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return control.httpClient.Do(req)
}

// keycloakStatusError 为 Admin API 失败生成有界且脱敏的诊断摘要；
// 必须在关闭响应体之前调用。状态码不符时，响应体刻意不按成功的 Keycloak
// 载荷解码，防止把错误页当成数据。
func keycloakStatusError(operation string, response *stdhttp.Response, accepted ...int) error {
	for _, status := range accepted {
		if response.StatusCode == status {
			return nil
		}
	}
	return fmt.Errorf("%s returned status %d: response=%s", operation, response.StatusCode, keycloakResponseSummary(response.Body))
}

func keycloakResponseSummary(body io.Reader) string {
	payload, err := io.ReadAll(io.LimitReader(body, keycloakAdminErrorSummaryLimit+1))
	if err != nil {
		return "<unavailable>"
	}
	truncated := len(payload) > keycloakAdminErrorSummaryLimit
	if truncated {
		payload = payload[:keycloakAdminErrorSummaryLimit]
	}
	summary := strings.Join(strings.Fields(string(payload)), " ")
	if summary == "" {
		return "<empty>"
	}
	// Keycloak 通常返回 JSON，但反向代理与自定义扩展可能返回纯文本 HTTP 头。
	// 必须先脱敏这类形式再执行通用键值匹配，防止 "Authorization: Bearer <token>"
	// 中的 token 跟着 "Bearer" 一词留在日志里。
	summary = keycloakAdminBearerToken.ReplaceAllString(summary, "$1<redacted>")
	summary = keycloakAdminCookieValue.ReplaceAllString(summary, "$1<redacted>")
	summary = keycloakAdminJWT.ReplaceAllString(summary, "<redacted>")
	summary = keycloakAdminSensitiveValue.ReplaceAllString(summary, "$1<redacted>")
	if truncated {
		summary += "…"
	}
	return summary
}
