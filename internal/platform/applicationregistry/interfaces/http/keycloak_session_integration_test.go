package http

import (
	"context"
	"encoding/json"
	"net"
	stdhttp "net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCentralSessionRealKeycloak 验证真实独立Keycloak中的受控策略投影和严格刷新协议。
// 参数：t为显式隔离容器测试上下文；管理凭据只从临时进程环境注入。
// 返回值：无；技术期限未收口、短令牌无法轮换或旧刷新材料可重用即失败。
func TestCentralSessionRealKeycloak(t *testing.T) {
	endpoint := os.Getenv("KEYCLOAK_SESSION_TEST_URL")
	if endpoint == "" {
		t.Skip("isolated Keycloak is not configured")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal("invalid isolated Keycloak URL")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || parsed.Scheme != "http" || host != "127.0.0.1" || port < 32768 || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" {
		t.Fatal("refusing non-isolated Keycloak")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	control := &keycloakControlPlane{adminURL: endpoint, realm: "qa-session-20261101", username: os.Getenv("KEYCLOAK_SESSION_TEST_USER"), password: os.Getenv("KEYCLOAK_SESSION_TEST_PASSWORD"), httpClient: &stdhttp.Client{Timeout: 5 * time.Second}, centralSessionGovernance: true}
	result, err := control.EnsureClientRedirectURIs(ctx, "qa-session-web", "Isolated session fixture", []string{"http://127.0.0.1/callback"})
	if err != nil {
		t.Fatal("controlled client provisioning failed", err)
	}
	admin, err := control.token(ctx)
	if err != nil {
		t.Fatal("isolated admin token failed")
	}
	var realm map[string]any
	keycloakSessionJSON(t, control, admin, "GET", "/admin/realms/qa-session-20261101", nil, &realm)
	for _, key := range []string{"ssoSessionIdleTimeout", "ssoSessionMaxLifespan", "clientSessionIdleTimeout", "clientSessionMaxLifespan"} {
		if realm[key] != float64(28800) {
			t.Fatalf("realm technical %s was not projected", key)
		}
	}
	var clients []struct {
		ID         string            `json:"id"`
		Attributes map[string]string `json:"attributes"`
	}
	keycloakSessionJSON(t, control, admin, "GET", "/admin/realms/qa-session-20261101/clients?clientId=qa-session-web", nil, &clients)
	if len(clients) != 1 {
		t.Fatal("expected one isolated client")
	}
	clientPath := "/admin/realms/qa-session-20261101/clients/" + clients[0].ID
	attributes := clients[0].Attributes
	if attributes["client.session.idle.timeout"] != "28800" || attributes["client.session.max.lifespan"] != "28800" {
		t.Fatal("client technical window was not projected")
	}
	// 只在一次性隔离Realm创建密码授权夹具，正式RP的标准授权码配置不改变。
	keycloakSessionJSON(t, control, admin, "PUT", "/admin/realms/qa-session-20261101", map[string]any{"revokeRefreshToken": true, "refreshTokenMaxReuse": 0, "accessTokenLifespan": 5}, nil)
	keycloakSessionJSON(t, control, admin, "PUT", clientPath, map[string]any{"directAccessGrantsEnabled": true}, nil)
	keycloakSessionJSON(t, control, admin, "POST", "/admin/realms/qa-session-20261101/users", map[string]any{"username": "qa-session-user", "enabled": true, "emailVerified": true, "email": "qa-session@example.invalid", "firstName": "Session", "lastName": "Fixture", "credentials": []map[string]any{{"type": "password", "temporary": false, "value": os.Getenv("KEYCLOAK_SESSION_TEST_PASSWORD")}}}, nil)
	tokenURL := endpoint + "/realms/qa-session-20261101/protocol/openid-connect/token"
	form := url.Values{"grant_type": {"password"}, "client_id": {result.ClientID}, "client_secret": {result.ClientSecret}, "username": {"qa-session-user"}, "password": {os.Getenv("KEYCLOAK_SESSION_TEST_PASSWORD")}, "scope": {"openid"}}
	request, err := stdhttp.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := control.httpClient.Do(request)
	if err != nil {
		t.Fatal("isolated token request failed")
	}
	defer response.Body.Close()
	var initial struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&initial) != nil || initial.RefreshToken == "" || initial.ExpiresIn != 5 {
		t.Fatalf("isolated short-token issuance failed: status=%d", response.StatusCode)
	}
	// 真实等待五秒访问令牌窗口，不以人工推进本地时钟冒充IdP失效。
	select {
	case <-ctx.Done():
		t.Fatal("fixture deadline exceeded")
	case <-time.After(6 * time.Second):
	}
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {result.ClientID}, "client_secret": {result.ClientSecret}, "refresh_token": {initial.RefreshToken}}
	rotated, status, code := postKeycloakSessionRefresh(t, control.httpClient, tokenURL, refreshForm)
	if status != 200 || rotated == "" || rotated == initial.RefreshToken || code != "" {
		t.Fatal("real Keycloak rotation failed")
	}
	_, status, code = postKeycloakSessionRefresh(t, control.httpClient, tokenURL, refreshForm)
	if status != 400 || code != "invalid_grant" {
		t.Fatal("consumed refresh token was accepted or failure misclassified")
	}
	t.Log("real Keycloak: managed 8h technical windows, actual 5s access expiry, refresh rotation and old-token rejection verified; browser code flow is separate")
}

// keycloakSessionJSON 仅向当前受守卫的隔离控制面发送夹具API请求。
// 参数：t为测试上下文，control为隔离控制面，token为管理材料，method/path为API，body为输入，result为输出。
// 返回值：无；只记录动作和HTTP状态，不输出凭据或响应正文。
func keycloakSessionJSON(t *testing.T, control *keycloakControlPlane, token, method, path string, body, result any) {
	t.Helper()
	response, err := control.request(context.Background(), token, method, path, body)
	if err != nil {
		t.Fatal("isolated Keycloak request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("isolated Keycloak %s failed: status=%d", method, response.StatusCode)
	}
	if result != nil && json.NewDecoder(response.Body).Decode(result) != nil {
		t.Fatal("isolated Keycloak returned invalid JSON")
	}
}

// postKeycloakSessionRefresh 调用真实隔离IdP的刷新端点，不将响应材料写入测试日志。
// 参数：t为测试上下文，client为有界HTTP客户端，endpoint为受守卫端点，form为内存中的测试材料。
// 返回值：新刷新材料、HTTP状态和协议错误码；网络或格式错误直接终止用例。
func postKeycloakSessionRefresh(t *testing.T, client *stdhttp.Client, endpoint string, form url.Values) (string, int, string) {
	t.Helper()
	response, err := client.PostForm(endpoint, form)
	if err != nil {
		t.Fatal("isolated refresh request failed")
	}
	defer response.Body.Close()
	var value struct {
		Refresh string `json:"refresh_token"`
		Error   string `json:"error"`
	}
	if json.NewDecoder(response.Body).Decode(&value) != nil {
		t.Fatal("isolated refresh response invalid")
	}
	return value.Refresh, response.StatusCode, value.Error
}
