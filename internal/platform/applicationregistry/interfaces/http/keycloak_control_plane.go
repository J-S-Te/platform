package http

// 本适配器只持有应用接入所需的 Keycloak Admin API 小型可审计子集。
// 刻意不把 Admin API 代理给浏览器，也绝不返回 access token 或客户端 Secret。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
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

// keycloakBrokerConfigurationError 标记 Keycloak 返回的确定性配置缺陷。
// 调用方可据此判断是否适合执行窄范围的 broker 凭据恢复；
// 传输与授权失败绝不允许触发凭据轮换。
type keycloakBrokerConfigurationError struct {
	alias      string
	missingKey string
}

func (err *keycloakBrokerConfigurationError) Error() string {
	return fmt.Sprintf("Keycloak IdP %s config incomplete: missing %s", err.alias, err.missingKey)
}

// IsRecoverableKeycloakBrokerConfigurationError 判断 Keycloak 是否返回了完整、
// 局部的配置缺陷。刻意排除网络、认证与授权失败，避免把环境故障误判为可恢复。
func IsRecoverableKeycloakBrokerConfigurationError(err error) bool {
	var configurationError *keycloakBrokerConfigurationError
	return errors.As(err, &configurationError)
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

const (
	keycloakDefaultRealmEventsExpirationSeconds = int64(30 * 24 * 60 * 60)
	keycloakRealmMaxLoginFailures               = int64(5)
	keycloakRealmTemporaryLockSeconds           = int64(15 * 60)
	keycloakRealmSSOIdleTimeoutSeconds          = int64(30 * 60)
)

var legacyKeycloakAuthorizationClaimNames = map[string]struct{}{
	"permissions":      {},
	"organization_ids": {},
	"role_config_hash": {},
	"authz_revision":   {},
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

func (control *keycloakControlPlane) ensureRealm(ctx context.Context, token string) error {
	response, err := control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm), nil)
	if err != nil {
		return err
	}
	if response.StatusCode == stdhttp.StatusOK {
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		// 兼容的 Admin API 代理可能返回 200 却不携带 Realm 表示。保持接入兼容；
		// 待完整响应可用时由调和流程强制设置项。
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		var realm map[string]any
		if err := json.Unmarshal(body, &realm); err != nil {
			return err
		}
		changed, updatePayload := realmEventSettingsPatch(realm)
		if !changed {
			return nil
		}
		response, err = control.request(ctx, token, stdhttp.MethodPut, "/admin/realms/"+url.PathEscape(control.realm), updatePayload)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("update Keycloak Realm", response, stdhttp.StatusNoContent); err != nil {
			return err
		}
		return nil
	}
	if response.StatusCode != stdhttp.StatusNotFound {
		defer response.Body.Close()
		return keycloakStatusError("read Keycloak Realm", response, stdhttp.StatusNotFound)
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms", map[string]any{
		"realm": control.realm, "enabled": true,
		"registrationAllowed": false, "verifyEmail": false,
		"bruteForceProtected": true, "permanentLockout": false,
		"failureFactor": keycloakRealmMaxLoginFailures, "waitIncrementSeconds": keycloakRealmTemporaryLockSeconds,
		"maxFailureWaitSeconds": keycloakRealmTemporaryLockSeconds,
		"ssoSessionIdleTimeout": keycloakRealmSSOIdleTimeoutSeconds,
		"eventsEnabled":         true, "adminEventsEnabled": true, "eventsExpiration": keycloakDefaultRealmEventsExpirationSeconds,
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("create Keycloak Realm", response, stdhttp.StatusCreated, stdhttp.StatusConflict); err != nil {
		return err
	}
	return nil
}

func realmEventSettingsPatch(realm map[string]any) (bool, map[string]any) {
	if realm == nil {
		return false, nil
	}
	payload := make(map[string]any, 12)
	changed := false
	payload["registrationAllowed"] = false
	payload["verifyEmail"] = false
	// Realm 关闭公开注册，并由 Keycloak 在认证入口统一执行失败锁定，
	// 避免各子系统分别统计失败次数而产生可绕过的安全边界。
	payload["bruteForceProtected"] = true
	payload["permanentLockout"] = false
	payload["failureFactor"] = keycloakRealmMaxLoginFailures
	payload["waitIncrementSeconds"] = keycloakRealmTemporaryLockSeconds
	payload["maxFailureWaitSeconds"] = keycloakRealmTemporaryLockSeconds
	payload["ssoSessionIdleTimeout"] = keycloakRealmSSOIdleTimeoutSeconds
	payload["eventsEnabled"] = true
	payload["adminEventsEnabled"] = true
	payload["eventsExpiration"] = keycloakDefaultRealmEventsExpirationSeconds
	if !asBool(realm["eventsEnabled"]) {
		changed = true
	}
	if !asBool(realm["adminEventsEnabled"]) {
		changed = true
	}
	if !asPositiveInt64(realm["eventsExpiration"]) {
		changed = true
	}
	if current, ok := realm["registrationAllowed"].(bool); !ok || current {
		changed = true
	}
	if current, ok := realm["verifyEmail"].(bool); !ok || current {
		changed = true
	}
	if !asBool(realm["bruteForceProtected"]) {
		changed = true
	}
	if current, ok := realm["permanentLockout"].(bool); !ok || current {
		changed = true
	}
	if !asInt64Equal(realm["failureFactor"], keycloakRealmMaxLoginFailures) {
		changed = true
	}
	if !asInt64Equal(realm["waitIncrementSeconds"], keycloakRealmTemporaryLockSeconds) {
		changed = true
	}
	if !asInt64Equal(realm["maxFailureWaitSeconds"], keycloakRealmTemporaryLockSeconds) {
		changed = true
	}
	if !asInt64Equal(realm["ssoSessionIdleTimeout"], keycloakRealmSSOIdleTimeoutSeconds) {
		changed = true
	}
	if changed {
		return true, payload
	}
	return false, nil
}

func asInt64Equal(value any, expected int64) bool {
	switch current := value.(type) {
	case int:
		return int64(current) == expected
	case int64:
		return current == expected
	case int32:
		return int64(current) == expected
	case int16:
		return int64(current) == expected
	case int8:
		return int64(current) == expected
	case float64:
		return current == float64(expected)
	case float32:
		return current == float32(expected)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(current), 10, 64)
		return err == nil && parsed == expected
	}
	return false
}

func asPositiveInt64(value any) bool {
	switch current := value.(type) {
	case int:
		return current > 0
	case int64:
		return current > 0
	case int32:
		return int64(current) > 0
	case int16:
		return int64(current) > 0
	case int8:
		return int64(current) > 0
	case float64:
		return int64(current) > 0
	case float32:
		return int64(current) > 0
	case string:
		trimmed := strings.TrimSpace(current)
		if trimmed == "" {
			return false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		return err == nil && parsed > 0
	}
	return false
}

func asBool(value any) bool {
	if value == nil {
		return false
	}
	current, ok := value.(bool)
	if !ok {
		return false
	}
	return current
}

func (control *keycloakControlPlane) brokerRepresentation(alias, displayName, clientID, clientSecret, backchannel string) map[string]any {
	return map[string]any{
		"alias": alias, "displayName": displayName, "providerId": "oidc",
		"enabled": true, "trustEmail": true, "storeToken": false,
		"updateProfileFirstLoginMode": "off",
		"firstBrokerLoginFlowAlias":   keycloakPrelinkedBrokerFlowAlias,
		"config": map[string]string{
			"clientId": clientID, "clientSecret": clientSecret, "clientAuthMethod": "client_secret_basic",
			"authorizationUrl": control.platformIssuer + "/authorize", "tokenUrl": backchannel + "/oauth2/token",
			"userInfoUrl": backchannel + "/oauth2/userinfo", "logoutUrl": control.platformIssuer + "/oauth2/logout",
			"defaultScope": "openid profile",
		},
	}
}

const (
	keycloakDefaultFirstBrokerLoginFlow = "first broker login"
	keycloakPrelinkedBrokerFlowAlias    = "basic-platform prelinked only"
	keycloakDenyAccessProviderID        = "deny-access-authenticator"
	keycloakReviewProfileProviderID     = "idp-review-profile"
	keycloakReviewProfileModeKey        = "update.profile.on.first.login"
)

// ensurePrelinkedBrokerFlow 创建提供方专属、失败关闭的首次登录流程。
// 对已关联的联合身份 Keycloak 会跳过该流程；未知主体只会命中唯一的 deny
// 执行项，而不会进入 Keycloak 的 JIT 用户/资料注册流程。
func (control *keycloakControlPlane) ensurePrelinkedBrokerFlow(ctx context.Context, token string) error {
	realmPath := "/admin/realms/" + url.PathEscape(control.realm)
	flowPath := realmPath + "/authentication/flows/" + url.PathEscape(keycloakPrelinkedBrokerFlowAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, flowPath, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status == stdhttp.StatusNotFound {
		response, err = control.request(ctx, token, stdhttp.MethodPost, realmPath+"/authentication/flows", map[string]any{
			"alias": keycloakPrelinkedBrokerFlowAlias, "description": "Basic Platform projected users only",
			"providerId": "basic-flow", "topLevel": true, "builtIn": false,
		})
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusCreated && status != stdhttp.StatusConflict {
			return fmt.Errorf("create prelinked-only Keycloak Broker flow returned HTTP %d", status)
		}
	} else if status != stdhttp.StatusOK {
		return fmt.Errorf("read prelinked-only Keycloak Broker flow returned HTTP %d", status)
	}

	type flowExecution struct {
		ID          string `json:"id"`
		ProviderID  string `json:"providerId"`
		Requirement string `json:"requirement"`
	}
	executionsPath := flowPath + "/executions"
	readExecutions := func() ([]flowExecution, error) {
		response, err := control.request(ctx, token, stdhttp.MethodGet, executionsPath, nil)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("read prelinked-only Keycloak Broker executions", response, stdhttp.StatusOK); err != nil {
			return nil, err
		}
		var executions []flowExecution
		if err := json.NewDecoder(response.Body).Decode(&executions); err != nil {
			return nil, err
		}
		return executions, nil
	}
	executions, err := readExecutions()
	if err != nil {
		return err
	}
	denyID := ""
	for _, execution := range executions {
		if execution.ProviderID == keycloakDenyAccessProviderID && denyID == "" {
			denyID = strings.TrimSpace(execution.ID)
			continue
		}
		if strings.TrimSpace(execution.ID) == "" {
			continue
		}
		response, err = control.request(ctx, token, stdhttp.MethodDelete, realmPath+"/authentication/executions/"+url.PathEscape(execution.ID), nil)
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusNoContent {
			return fmt.Errorf("remove drifted prelinked-only Broker execution returned HTTP %d", status)
		}
	}
	if denyID == "" {
		response, err = control.request(ctx, token, stdhttp.MethodPost, flowPath+"/executions/execution", map[string]string{"provider": keycloakDenyAccessProviderID})
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != stdhttp.StatusCreated {
			return fmt.Errorf("add deny execution to prelinked-only Broker flow returned HTTP %d", status)
		}
		executions, err = readExecutions()
		if err != nil {
			return err
		}
		for _, execution := range executions {
			if execution.ProviderID == keycloakDenyAccessProviderID {
				denyID = strings.TrimSpace(execution.ID)
				break
			}
		}
	}
	if denyID == "" {
		return fmt.Errorf("prelinked-only Keycloak Broker flow has no deny execution")
	}
	response, err = control.request(ctx, token, stdhttp.MethodPut, executionsPath, map[string]string{"id": denyID, "requirement": "REQUIRED"})
	if err != nil {
		return err
	}
	status = response.StatusCode
	response.Body.Close()
	if status != stdhttp.StatusNoContent {
		return fmt.Errorf("require deny execution in prelinked-only Broker flow returned HTTP %d", status)
	}
	executions, err = readExecutions()
	if err != nil {
		return err
	}
	if len(executions) != 1 || executions[0].ProviderID != keycloakDenyAccessProviderID || executions[0].Requirement != "REQUIRED" {
		return fmt.Errorf("prelinked-only Keycloak Broker flow did not converge to one required deny execution")
	}
	return nil
}

// ensureBrokerReviewProfileDisabled 调和 Keycloak 26 的实际生效设置。
// IdentityProvider 表示上的 updateProfileFirstLoginMode 仅为兼容保留；
// 当前版本从首个 Broker 流程的 Review Profile authenticator 执行项读取该模式。
func (control *keycloakControlPlane) ensureBrokerReviewProfileDisabled(ctx context.Context, token, brokerAlias string) error {
	realmPath := "/admin/realms/" + url.PathEscape(control.realm)
	providerPath := realmPath + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, providerPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read effective Keycloak Broker flow", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var provider struct {
		FirstBrokerLoginFlowAlias string `json:"firstBrokerLoginFlowAlias"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&provider)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	flowAlias := strings.TrimSpace(provider.FirstBrokerLoginFlowAlias)
	if flowAlias == "" {
		response, err = control.request(ctx, token, stdhttp.MethodGet, realmPath, nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("read Keycloak Realm Broker flow", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return err
		}
		var realm struct {
			FirstBrokerLoginFlow string `json:"firstBrokerLoginFlow"`
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&realm)
		response.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		flowAlias = strings.TrimSpace(realm.FirstBrokerLoginFlow)
		if flowAlias == "" {
			flowAlias = keycloakDefaultFirstBrokerLoginFlow
		}
	}
	executionsPath := realmPath + "/authentication/flows/" + url.PathEscape(flowAlias) + "/executions"
	response, err = control.request(ctx, token, stdhttp.MethodGet, executionsPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read Keycloak first Broker login executions", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var executions []struct {
		ID                   string `json:"id"`
		ProviderID           string `json:"providerId"`
		AuthenticationConfig string `json:"authenticationConfig"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&executions)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	for _, execution := range executions {
		if execution.ProviderID != keycloakReviewProfileProviderID || strings.TrimSpace(execution.ID) == "" {
			continue
		}
		configID := strings.TrimSpace(execution.AuthenticationConfig)
		if configID == "" {
			payload := map[string]any{"alias": "basic-platform-review-profile", "config": map[string]string{keycloakReviewProfileModeKey: "off"}}
			response, err = control.request(ctx, token, stdhttp.MethodPost, realmPath+"/authentication/executions/"+url.PathEscape(execution.ID)+"/config", payload)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return keycloakStatusError("configure Keycloak Broker Review Profile", response, stdhttp.StatusCreated)
		}
		configPath := realmPath + "/authentication/config/" + url.PathEscape(configID)
		response, err = control.request(ctx, token, stdhttp.MethodGet, configPath, nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("read Keycloak Broker Review Profile config", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return err
		}
		var config struct {
			ID     string            `json:"id,omitempty"`
			Alias  string            `json:"alias"`
			Config map[string]string `json:"config"`
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&config)
		response.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if config.Config[keycloakReviewProfileModeKey] == "off" {
			return nil
		}
		if config.Config == nil {
			config.Config = map[string]string{}
		}
		if strings.TrimSpace(config.Alias) == "" {
			config.Alias = "basic-platform-review-profile"
		}
		config.ID = configID
		config.Config[keycloakReviewProfileModeKey] = "off"
		response, err = control.request(ctx, token, stdhttp.MethodPut, configPath, config)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		return keycloakStatusError("update Keycloak Broker Review Profile config", response, stdhttp.StatusNoContent)
	}
	// 不含 Review Profile 的自定义流程本身已满足"零交互"契约，无需再配置 authenticator。
	return nil
}

// ensureBrokerUserProfile 声明从基础平台导入的属性。Keycloak 26 会按 Realm
// user-profile 模式校验 mapper 目标，Attribute Importer 无法在首次登录时
// 任意创建属性，因此必须先声明。
func (control *keycloakControlPlane) ensureBrokerUserProfile(ctx context.Context, token string) error {
	path := "/admin/realms/" + url.PathEscape(control.realm) + "/users/profile"
	response, err := control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("read Keycloak user profile", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var profile map[string]any
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		return err
	}
	attributes, _ := profile["attributes"].([]any)
	// 资料校验归基础平台所有；Keycloak 不得把可选的平台字段变成首次登录注册表单。
	for index, raw := range attributes {
		attribute, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch attribute["name"] {
		case "email", "firstName", "lastName":
			delete(attribute, "required")
			attributes[index] = attribute
		}
	}
	for _, claim := range keycloakIdentityClaimMappings {
		found := false
		for index, raw := range attributes {
			attribute, ok := raw.(map[string]any)
			if !ok || attribute["name"] != claim.Name {
				continue
			}
			// 每次同步都修复被手工改动的资料元数据，防止 Keycloak 管理员把列表 claim
			// 静默改成标量，或把映射改成用户可编辑。
			attribute["displayName"] = claim.Name
			attribute["multivalued"] = claim.MultiValued
			attribute["permissions"] = map[string]any{"view": []string{"admin"}, "edit": []string{"admin"}}
			attributes[index] = attribute
			found = true
			break
		}
		if found {
			continue
		}
		attributes = append(attributes, map[string]any{
			"name":        claim.Name,
			"displayName": claim.Name,
			"multivalued": claim.MultiValued,
			"permissions": map[string]any{"view": []string{"admin"}, "edit": []string{"admin"}},
		})
	}
	profile["attributes"] = attributes
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodPut, path, profile)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("configure Keycloak user profile", response, stdhttp.StatusOK, stdhttp.StatusNoContent); err != nil {
		return err
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("verify Keycloak user profile", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var verified map[string]any
	if err := json.NewDecoder(response.Body).Decode(&verified); err != nil {
		return err
	}
	verifiedAttributes, _ := verified["attributes"].([]any)
	for _, raw := range verifiedAttributes {
		attribute, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := attribute["name"].(string)
		if name != "email" && name != "firstName" && name != "lastName" {
			continue
		}
		if required, exists := attribute["required"]; exists && required != nil {
			if encoded, _ := json.Marshal(required); string(encoded) != "{}" && string(encoded) != "[]" && string(encoded) != "false" && string(encoded) != "null" {
				return fmt.Errorf("Keycloak user profile field %s remained required", name)
			}
		}
	}
	return nil
}

// EnsureBroker 注册上游基础平台身份提供方，并把授权 claims 导入 Keycloak 用户属性。
// 别名刻意显式区分：内部员工与外部客户使用不同 Broker 客户端，
// 一个应用的回调无法覆盖另一个应用的客户端凭据。
func (control *keycloakControlPlane) EnsureBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform", "基础平台", clientID, clientSecret)
}

// EnsureCustomerPortalBroker 配置与内部 Broker 隔离的客户门户 IdP，凭据互不复用。
func (control *keycloakControlPlane) EnsureCustomerPortalBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform-customer", "客户自助门户", clientID, clientSecret)
}

// VerifyBrokerExists 检查 basic-platform IdP 存在且配置完整；不修改 IdP，
// 也不需要客户端 Secret。适用于平台已持有有效凭据、仅需确认 Keycloak 侧健康的场景。
func (control *keycloakControlPlane) VerifyBrokerExists(ctx context.Context) error {
	return control.verifyBrokerExists(ctx, "basic-platform")
}

// VerifyCustomerPortalBrokerExists 检查客户门户 IdP 的健康状态。
func (control *keycloakControlPlane) VerifyCustomerPortalBrokerExists(ctx context.Context) error {
	return control.verifyBrokerExists(ctx, "basic-platform-customer")
}

func (control *keycloakControlPlane) verifyBrokerExists(ctx context.Context, brokerAlias string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return err
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == stdhttp.StatusNotFound {
		// 平台对健康的 broker 凭据只保存哈希，因此重建被删除的 IdP 必须走窄范围的
		// 凭据恢复路径。该别名的 404 属于确定性的 Keycloak 配置漂移，与传输、认证或
		// 权限失败不同，因此可安全标记为可恢复。
		return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: "identity-provider"}
	}
	if err := keycloakStatusError("read Keycloak IdP", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var verified struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&verified); err != nil {
		return fmt.Errorf("decode Keycloak IdP %s: %w", brokerAlias, err)
	}
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	for _, key := range requiredKeys {
		if strings.TrimSpace(verified.Config[key]) == "" {
			return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: key}
		}
	}
	return nil
}

// KeycloakDriftReport 描述单个子系统客户端在"平台期望状态"与"Keycloak 实际状态"
// 之间检测到的配置差异，各布尔字段即对应检查项的符合性。
type KeycloakDriftReport struct {
	ClientID       string   `json:"client_id"`
	HasDrift       bool     `json:"has_drift"`
	MissingRoles   []string `json:"missing_roles,omitempty"`
	StaleRoles     []string `json:"stale_roles,omitempty"`
	MissingMappers []string `json:"missing_mappers,omitempty"`
	DriftedMappers []string `json:"drifted_mappers,omitempty"`
	RedirectURIOK  bool     `json:"redirect_uri_ok"`
	BrokerConfigOK bool     `json:"broker_config_ok"`
}

func brokerAliasForClient(clientID string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(clientID)), "customer_portal-") {
		return "basic-platform-customer"
	}
	return "basic-platform"
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// DetectSubsystemKeycloakDrift 将子系统客户端的期望 Keycloak 配置与实际状态比对，检查：
//   - 客户端存在性与回调 URI
//   - 角色目录完整性
//   - 协议 mapper 的存在与配置
//   - Broker IdP 配置完整性
//
// 本方法只读，不修改任何 Keycloak 状态；结果仅用于决定是否触发调和。
func (control *keycloakControlPlane) DetectSubsystemKeycloakDrift(ctx context.Context, clientID, expectedRedirectURI string, expectedRoleCodes []string) (KeycloakDriftReport, error) {
	report := KeycloakDriftReport{ClientID: clientID, RedirectURIOK: true, BrokerConfigOK: true}
	token, err := control.token(ctx)
	if err != nil {
		return report, err
	}
	// 检查客户端是否存在。
	lookupPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	response, err := control.request(ctx, token, stdhttp.MethodGet, lookupPath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var clients []struct {
		ID           string   `json:"id"`
		RedirectURIs []string `json:"redirectUris"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&clients)
	response.Body.Close()
	if decodeErr != nil {
		return report, fmt.Errorf("decode Keycloak Client for drift check: %w", decodeErr)
	}
	if len(clients) == 0 {
		report.HasDrift = true
		return report, nil
	}
	internalID := clients[0].ID
	// 检查回调 URI。
	if expectedRedirectURI != "" {
		found := false
		for _, uri := range clients[0].RedirectURIs {
			if uri == expectedRedirectURI {
				found = true
				break
			}
		}
		if !found {
			report.RedirectURIOK = false
			report.HasDrift = true
		}
	}
	// 检查角色目录。
	rolePath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(internalID) + "/roles"
	response, err = control.request(ctx, token, stdhttp.MethodGet, rolePath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client roles for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var existingRoles []struct {
		Name string `json:"name"`
	}
	decodeRolesErr := json.NewDecoder(response.Body).Decode(&existingRoles)
	response.Body.Close()
	if decodeRolesErr != nil {
		return report, fmt.Errorf("decode Keycloak Client roles for drift check: %w", decodeRolesErr)
	}
	existingRoleSet := make(map[string]struct{}, len(existingRoles))
	for _, role := range existingRoles {
		existingRoleSet[role.Name] = struct{}{}
	}
	for _, code := range expectedRoleCodes {
		if _, exists := existingRoleSet[code]; !exists {
			report.MissingRoles = append(report.MissingRoles, code)
			report.HasDrift = true
		}
	}
	expectedRoleSet := make(map[string]struct{}, len(expectedRoleCodes))
	for _, code := range expectedRoleCodes {
		expectedRoleSet[code] = struct{}{}
	}
	for name := range existingRoleSet {
		if _, expected := expectedRoleSet[name]; !expected {
			report.StaleRoles = append(report.StaleRoles, name)
			report.HasDrift = true
		}
	}
	sort.Strings(report.MissingRoles)
	sort.Strings(report.StaleRoles)
	// 按名称与配置双维度比较平台管控的协议 mapper；
	// 名称相同但 claim 已变的 mapper 仍视为漂移。
	mapperPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(internalID) + "/protocol-mappers/models"
	response, err = control.request(ctx, token, stdhttp.MethodGet, mapperPath, nil)
	if err != nil {
		return report, err
	}
	if err := keycloakStatusError("read Keycloak Client mappers for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var existingMappers []struct {
		ID             string            `json:"id"`
		Name           string            `json:"name"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	decodeMappersErr := json.NewDecoder(response.Body).Decode(&existingMappers)
	response.Body.Close()
	if decodeMappersErr != nil {
		return report, fmt.Errorf("decode Keycloak Client mappers for drift check: %w", decodeMappersErr)
	}
	actualMappers := make(map[string]struct {
		ProtocolMapper string
		Config         map[string]string
	}, len(existingMappers))
	for _, mapper := range existingMappers {
		actualMappers[mapper.Name] = struct {
			ProtocolMapper string
			Config         map[string]string
		}{mapper.ProtocolMapper, mapper.Config}
	}
	for _, expected := range keycloakAuthorizationProtocolMappers(clientID) {
		actual, ok := actualMappers[expected.Name]
		if !ok {
			report.MissingMappers = append(report.MissingMappers, expected.Name)
			report.HasDrift = true
			continue
		}
		if actual.ProtocolMapper != expected.ProtocolMapper || !mapsEqual(actual.Config, expected.Config) {
			report.DriftedMappers = append(report.DriftedMappers, expected.Name)
			report.HasDrift = true
		}
	}
	sort.Strings(report.MissingMappers)
	sort.Strings(report.DriftedMappers)
	// 检查 broker 配置。
	brokerAlias := brokerAliasForClient(clientID)
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances/"+url.PathEscape(brokerAlias), nil)
	if err != nil {
		// 管理 API 不可达不等同于“配置正常”。向上返回错误，让健康仪表盘明确显示
		// DRIFT_DETECTION_FAILED，禁止用默认 BrokerConfigOK=true 制造假绿状态。
		return report, fmt.Errorf("read Keycloak Broker for drift check: %w", err)
	}
	if err := keycloakStatusError("read Keycloak Broker for drift check", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return report, err
	}
	var broker struct {
		Config map[string]string `json:"config"`
	}
	decodeBrokerErr := json.NewDecoder(response.Body).Decode(&broker)
	response.Body.Close()
	if decodeBrokerErr != nil {
		return report, fmt.Errorf("decode Keycloak Broker for drift check: %w", decodeBrokerErr)
	}
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	for _, key := range requiredKeys {
		if strings.TrimSpace(broker.Config[key]) == "" {
			report.BrokerConfigOK = false
			report.HasDrift = true
			break
		}
	}
	return report, nil
}

func (control *keycloakControlPlane) ensureBroker(ctx context.Context, brokerAlias, displayName, clientID, clientSecret string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return err
	}
	if err := control.ensurePrelinkedBrokerFlow(ctx, token); err != nil {
		return err
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/identity-provider/instances/" + url.PathEscape(brokerAlias)
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	exists := response.StatusCode == stdhttp.StatusOK
	if !exists && response.StatusCode != stdhttp.StatusNotFound {
		defer response.Body.Close()
		return keycloakStatusError("read Keycloak broker", response, stdhttp.StatusOK, stdhttp.StatusNotFound)
	}
	response.Body.Close()
	backchannel := control.platformBackchannel
	if backchannel == "" {
		backchannel = control.platformIssuer
	}
	// 面向浏览器的端点必须使用公开 Issuer；只有 token 与 userinfo 调用是服务端到
	// 服务端，可走 backchannel。把内部 Docker 主机名暴露为 logoutUrl 会让
	// Keycloak 在 Broker 登出期间把用户浏览器重定向到不可达主机。
	// 用户在进入子系统之前已由平台授权投影完成供给与关联；绝不暴露 Keycloak 的
	// 首次登录资料补全表单——邮箱与拆分的姓名在平台侧是可选字段，
	// 不能变成另一套注册契约。
	payload := control.brokerRepresentation(brokerAlias, displayName, clientID, clientSecret, backchannel)
	if exists {
		response, err = control.request(ctx, token, stdhttp.MethodPut, base, payload)
	} else {
		response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances", payload)
	}
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("configure Keycloak broker", response, stdhttp.StatusNoContent, stdhttp.StatusCreated); err != nil {
		return err
	}
	response.Body.Close()
	response, err = control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("verify Keycloak broker", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var verifiedBroker struct {
		FirstBrokerLoginFlowAlias string            `json:"firstBrokerLoginFlowAlias"`
		Config                    map[string]string `json:"config"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&verifiedBroker)
	response.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	// 自愈：核验 IdP 配置完整性。PUT 期间的时序竞争或缓存未命中可能让 Keycloak
	// 返回空 config{}。重新提交一次载荷；仍失败则把错误上抛，由 Worker 在下次
	// 启动时重试，而不是静默留下一个损坏的 IdP。
	if err := control.ensureBrokerConfigComplete(ctx, token, base, brokerAlias, payload, verifiedBroker.Config); err != nil {
		return err
	}
	// Keycloak 26 可能持续把遗留的顶层 profile 模式报为 "on"；
	// 提供方专属的 deny-only 流程才是权威的"不注册"契约。
	if verifiedBroker.FirstBrokerLoginFlowAlias != keycloakPrelinkedBrokerFlowAlias {
		return fmt.Errorf("Keycloak Broker policy did not converge to prelinked-only mode")
	}
	if err := control.ensureBrokerReviewProfileDisabled(ctx, token, brokerAlias); err != nil {
		return err
	}
	if err := control.ensureBrokerUserProfile(ctx, token); err != nil {
		return err
	}
	type existingMapper struct {
		ID     string
		Config map[string]string
	}
	listMappers := func() (map[string]existingMapper, error) {
		response, err := control.request(ctx, token, stdhttp.MethodGet, base+"/mappers", nil)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if err := keycloakStatusError("read Keycloak broker mappers", response, stdhttp.StatusOK); err != nil {
			return nil, err
		}
		var mappers []struct {
			ID     string            `json:"id"`
			Name   string            `json:"name"`
			Config map[string]string `json:"config"`
		}
		if err := json.NewDecoder(response.Body).Decode(&mappers); err != nil {
			return nil, err
		}
		ids := make(map[string]existingMapper, len(mappers))
		for _, mapper := range mappers {
			ids[mapper.Name] = existingMapper{ID: mapper.ID, Config: mapper.Config}
		}
		return ids, nil
	}
	mapperIDs, err := listMappers()
	if err != nil {
		return err
	}
	for _, claim := range keycloakIdentityClaimMappings {
		mapper := map[string]any{"name": brokerAlias + "-" + claim.Name, "identityProviderAlias": brokerAlias, "identityProviderMapper": "oidc-user-attribute-idp-mapper", "config": map[string]string{"syncMode": "INHERIT", "claim": claim.Name, "user.attribute": claim.Name}}
		mapperName := brokerAlias + "-" + claim.Name
		existing := mapperIDs[mapperName]
		mapperID := existing.ID
		if mapperID != "" && existing.Config["syncMode"] == "INHERIT" && existing.Config["claim"] == claim.Name && existing.Config["user.attribute"] == claim.Name {
			continue
		}
		method, path := stdhttp.MethodPost, base+"/mappers"
		if mapperID != "" {
			// Keycloak 26 的缓存层可能拒绝就地更新 mapper，即使记录本身有效。
			// 只对发生漂移的 mapper 执行删除重建，避免放大影响面。
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/mappers/"+url.PathEscape(mapperID), nil)
			if err != nil {
				return err
			}
			if err := keycloakStatusError("delete Keycloak broker claim "+claim.Name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
				response.Body.Close()
				return err
			}
			response.Body.Close()
		}
		response, err = control.request(ctx, token, method, path, mapper)
		if err != nil {
			return err
		}
		if response.StatusCode == stdhttp.StatusCreated {
			response.Body.Close()
			continue
		}
		createErr := keycloakStatusError("configure Keycloak broker claim "+claim.Name, response, stdhttp.StatusCreated)
		response.Body.Close()
		// Keycloak 26.2 可能在落库 IdP mapper 后、收尾 Admin 事件时返回 400。
		// 此时重读并更新该 mapper，使操作保持正确且可安全重试，
		// 而不是对外暴露一个虚假的 503。
		mapperIDs, err = listMappers()
		if err != nil {
			return err
		}
		existing = mapperIDs[mapperName]
		mapperID = existing.ID
		if mapperID == "" {
			return createErr
		}
		if existing.Config["syncMode"] != "INHERIT" || existing.Config["claim"] != claim.Name || existing.Config["user.attribute"] != claim.Name {
			return createErr
		}
	}
	return nil
}

// ensureBrokerConfigComplete 核验从 Keycloak 读回的 IdP 配置是否包含全部必需字段；
// 配置为空或缺关键键时重新提交一次期望载荷。该自愈针对 Keycloak 已知竞态：
// PUT 返回 204 但配置仍为空 {}。
func (control *keycloakControlPlane) ensureBrokerConfigComplete(ctx context.Context, token, base, brokerAlias string, expected map[string]any, actual map[string]string) error {
	requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
	complete := true
	for _, key := range requiredKeys {
		if strings.TrimSpace(actual[key]) == "" {
			complete = false
			break
		}
	}
	if complete {
		return nil
	}
	// 重新提交一次期望配置。
	response, err := control.request(ctx, token, stdhttp.MethodPut, base, expected)
	if err != nil {
		return fmt.Errorf("self-heal Keycloak IdP %s config: %w", brokerAlias, err)
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return fmt.Errorf("self-heal Keycloak IdP %s config returned HTTP %d", brokerAlias, response.StatusCode)
	}
	// 核验修复已生效。
	response, err = control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return fmt.Errorf("verify self-healed Keycloak IdP %s config: %w", brokerAlias, err)
	}
	defer response.Body.Close()
	if err := keycloakStatusError("verify self-healed Keycloak IdP config", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var recheck struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&recheck); err != nil {
		return fmt.Errorf("decode self-healed Keycloak IdP %s config: %w", brokerAlias, err)
	}
	for _, key := range requiredKeys {
		if strings.TrimSpace(recheck.Config[key]) == "" {
			return &keycloakBrokerConfigurationError{alias: brokerAlias, missingKey: key}
		}
	}
	return nil
}

// EnsureClient 创建/更新机密 RP 及既有子系统期望的 claims。来自上游平台 IdP 的
// 属性导入必须先单独配置完成，本方法才允许用于生产切换。
func (control *keycloakControlPlane) EnsureClient(ctx context.Context, clientID, name, redirectURI string) (keycloakClientResult, error) {
	return control.EnsureClientRedirectURIs(ctx, clientID, name, []string{redirectURI})
}

// EnsureClientRedirectURIs 原子替换单个 Realm 客户端的全部平台管控回调集合。
// 传输层切换期间先配置两个回调等流量排空，再以单一目标回调重新调用。
// 除子系统接入所需的既有 Secret 读取外，本方法不读取也不改动任何无关客户端凭据。
func (control *keycloakControlPlane) EnsureClientRedirectURIs(ctx context.Context, clientID, name string, redirectURIs []string) (keycloakClientResult, error) {
	redirectURIs = canonicalRedirectURIs(redirectURIs)
	if len(redirectURIs) == 0 {
		return keycloakClientResult{}, errors.New("at least one Keycloak redirect URI is required")
	}
	token, err := control.token(ctx)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return keycloakClientResult{}, err
	}
	path := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	response, err := control.request(ctx, token, stdhttp.MethodGet, path, nil)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := keycloakStatusError("read Keycloak Client", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return keycloakClientResult{}, err
	}
	var clients []struct {
		ID string `json:"id"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&clients)
	response.Body.Close()
	if decodeErr != nil {
		return keycloakClientResult{}, decodeErr
	}
	payload := map[string]any{"clientId": clientID, "name": name, "enabled": true, "protocol": "openid-connect", "publicClient": false, "standardFlowEnabled": true, "directAccessGrantsEnabled": false, "redirectUris": redirectURIs, "webOrigins": []string{"+"}}
	internalID := ""
	if len(clients) == 0 {
		response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms/"+url.PathEscape(control.realm)+"/clients", payload)
		if err != nil {
			return keycloakClientResult{}, err
		}
		if err := keycloakStatusError("create Keycloak Client", response, stdhttp.StatusCreated); err != nil {
			response.Body.Close()
			return keycloakClientResult{}, err
		}
		response.Body.Close()
		response, err = control.request(ctx, token, stdhttp.MethodGet, path, nil)
		if err != nil {
			return keycloakClientResult{}, err
		}
		if err := keycloakStatusError("read newly created Keycloak Client", response, stdhttp.StatusOK); err != nil {
			response.Body.Close()
			return keycloakClientResult{}, err
		}
		decodeErr = json.NewDecoder(response.Body).Decode(&clients)
		response.Body.Close()
		if decodeErr != nil || len(clients) == 0 {
			return keycloakClientResult{}, fmt.Errorf("read newly created Keycloak Client failed")
		}
	} else {
		internalID = clients[0].ID
	}
	if internalID == "" {
		internalID = clients[0].ID
	}
	response, err = control.request(ctx, token, stdhttp.MethodPut, "/admin/realms/"+url.PathEscape(control.realm)+"/clients/"+url.PathEscape(internalID), payload)
	if err != nil {
		return keycloakClientResult{}, err
	}
	if err := keycloakStatusError("update Keycloak Client", response, stdhttp.StatusNoContent); err != nil {
		response.Body.Close()
		return keycloakClientResult{}, err
	}
	response.Body.Close()
	if err := control.ensureClaimMappers(ctx, token, internalID, clientID); err != nil {
		return keycloakClientResult{}, err
	}
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/clients/"+url.PathEscape(internalID)+"/client-secret", nil)
	if err != nil {
		return keycloakClientResult{}, err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("read Keycloak Client secret", response, stdhttp.StatusOK); err != nil {
		return keycloakClientResult{}, err
	}
	var secret struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&secret); err != nil {
		return keycloakClientResult{}, err
	}
	if secret.Value == "" {
		return keycloakClientResult{}, fmt.Errorf("Keycloak Client secret is empty")
	}
	return keycloakClientResult{ClientID: clientID, ClientSecret: secret.Value}, nil
}

func canonicalRedirectURIs(values []string) []string {
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

// EnsureClientRoles 把平台发布的角色目录镜像到目标 Keycloak 客户端。
// 角色始终按客户端命名空间隔离，合同管理里的 "admin" 角色绝不可能授予
// CRM 或项目系统的访问权限。
func (control *keycloakControlPlane) EnsureClientRoles(ctx context.Context, clientID string, roleCodes []string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	lookupPath := "/admin/realms/" + url.PathEscape(control.realm) + "/clients?clientId=" + url.QueryEscape(clientID)
	lookup, err := control.request(ctx, token, stdhttp.MethodGet, lookupPath, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("read Keycloak Client for roles", lookup, stdhttp.StatusOK); err != nil {
		lookup.Body.Close()
		return err
	}
	var clients []struct {
		ID string `json:"id"`
	}
	decodeErr := json.NewDecoder(lookup.Body).Decode(&clients)
	lookup.Body.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if len(clients) == 0 || strings.TrimSpace(clients[0].ID) == "" {
		return fmt.Errorf("Keycloak Client %s was not found", clientID)
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clients[0].ID) + "/roles"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := keycloakStatusError("list Keycloak Client roles", response, stdhttp.StatusOK); err != nil {
		return err
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&existing); err != nil {
		return err
	}
	known := make(map[string]struct{}, len(existing))
	for _, role := range existing {
		known[strings.TrimSpace(role.Name)] = struct{}{}
	}
	for _, raw := range roleCodes {
		code := strings.TrimSpace(raw)
		if code == "" {
			continue
		}
		if _, exists := known[code]; exists {
			continue
		}
		created, requestErr := control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": code, "description": "Managed by Basic Platform"})
		if requestErr != nil {
			return requestErr
		}
		if err := keycloakStatusError("create Keycloak Client role "+code, created, stdhttp.StatusCreated, stdhttp.StatusConflict); err != nil {
			created.Body.Close()
			return err
		}
		created.Body.Close()
	}
	return nil
}

func (control *keycloakControlPlane) ensureClaimMappers(ctx context.Context, token, clientID, publicClientID string) error {
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clientID) + "/protocol-mappers/models"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if err := keycloakStatusError("list Keycloak claim mappers", response, stdhttp.StatusOK); err != nil {
		response.Body.Close()
		return err
	}
	var existing []struct {
		ID             string            `json:"id"`
		Name           string            `json:"name"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	err = json.NewDecoder(response.Body).Decode(&existing)
	response.Body.Close()
	if err != nil {
		return err
	}
	known := map[string]struct {
		ID             string
		ProtocolMapper string
		Config         map[string]string
	}{}
	for _, mapper := range existing {
		known[mapper.Name] = struct {
			ID             string
			ProtocolMapper string
			Config         map[string]string
		}{ID: mapper.ID, ProtocolMapper: mapper.ProtocolMapper, Config: mapper.Config}
	}
	managedMappers := make([]keycloakManagedProtocolMapper, 0, len(keycloakIdentityClaimMappings)+3)
	for _, claim := range keycloakIdentityClaimMappings {
		// identity_id 由下方的显式稳定 mapper 管理。遗留的通用投影 platform-identity_id
		// 曾把该属性映射到保留的 OIDC `sub` claim，覆盖了规范 subject，
		// 导致门户拒绝本应有效的授权码登录。
		if claim.Name == "identity_id" {
			continue
		}
		managedMappers = append(managedMappers, keycloakManagedProtocolMapper{
			Name:           "platform-" + claim.Name,
			ProtocolMapper: "oidc-usermodel-attribute-mapper",
			Config:         keycloakIdentityClaimMapperConfig(claim),
		})
	}
	managedMappers = append(managedMappers, keycloakAuthorizationProtocolMappers(publicClientID)...)
	managedNames := make(map[string]struct{}, len(managedMappers))
	for _, mapper := range managedMappers {
		managedNames[mapper.Name] = struct{}{}
	}

	// 移除过期的平台管控 mapper 与历史上精确的明细授权 Claim。按 claim.name 精确
	// 匹配而非按 mapper 名称模糊匹配，可以在不触碰第三方资料/角色 mapper 的
	// 前提下迁移手工命名的遗留 mapper。
	for name, current := range known {
		if _, keep := managedNames[name]; keep {
			continue
		}
		_, legacyDetailedAuthorization := legacyKeycloakAuthorizationClaimNames[strings.TrimSpace(current.Config["claim.name"])]
		if name != "platform-token-use" && !strings.HasPrefix(name, "platform-") && !legacyDetailedAuthorization {
			continue
		}
		response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
		if err != nil {
			return err
		}
		if err := keycloakStatusError("delete stale Keycloak claim mapper "+name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
			response.Body.Close()
			return err
		}
		response.Body.Close()
		delete(known, name)
	}

	for _, mapper := range managedMappers {
		current, exists := known[mapper.Name]
		if exists && current.ProtocolMapper == mapper.ProtocolMapper && keycloakMapperConfigMatches(current.Config, mapper.Config) {
			continue
		}
		if exists {
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
			if err != nil {
				return err
			}
			if err := keycloakStatusError("delete drifted Keycloak mapper "+mapper.Name, response, stdhttp.StatusNoContent, stdhttp.StatusNotFound); err != nil {
				response.Body.Close()
				return err
			}
			response.Body.Close()
		}
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": mapper.Name, "protocol": "openid-connect", "protocolMapper": mapper.ProtocolMapper, "config": mapper.Config})
		if err != nil {
			return err
		}
		if err := keycloakStatusError("create Keycloak mapper "+mapper.Name, response, stdhttp.StatusCreated); err != nil {
			response.Body.Close()
			return err
		}
		response.Body.Close()
	}
	return nil
}

func keycloakAuthorizationProtocolMappers(publicClientID string) []keycloakManagedProtocolMapper {
	return []keycloakManagedProtocolMapper{
		{
			// 让稳定平台身份同时以其显式契约名 identity_id 与 OIDC sub 两个键可用：
			// 既有依赖方使用 identity_id，sub 保持为规范 OIDC subject。
			Name:           "platform-stable-identity-id",
			ProtocolMapper: "oidc-usermodel-attribute-mapper",
			Config: map[string]string{
				"user.attribute": "identity_id", "claim.name": "identity_id", "jsonType.label": "String", "multivalued": "false",
				"id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "introspection.token.claim": "true",
			},
		},
		{
			Name:           "platform-token-use-id",
			ProtocolMapper: "oidc-hardcoded-claim-mapper",
			Config: map[string]string{
				"claim.name": "token_use", "claim.value": "id_token", "claim.value.type": "String",
				"id.token.claim": "true", "access.token.claim": "false", "userinfo.token.claim": "false",
			},
		},
		{
			Name:           "platform-token-use-access",
			ProtocolMapper: "oidc-hardcoded-claim-mapper",
			Config: map[string]string{
				"claim.name": "token_use", "claim.value": "access_token", "claim.value.type": "String",
				"id.token.claim": "false", "access.token.claim": "true", "userinfo.token.claim": "false",
			},
		},
		{
			Name:           "platform-client-audience",
			ProtocolMapper: "oidc-audience-mapper",
			Config: map[string]string{
				"included.custom.audience": publicClientID,
				"id.token.claim":           "false",
				"access.token.claim":       "true",
			},
		},
	}
}

func keycloakIdentityClaimMapperConfig(claim keycloakIdentityClaimMapping) map[string]string {
	claimName := claim.ClaimName
	if claimName == "" {
		claimName = claim.Name
	}
	config := map[string]string{"user.attribute": claim.Name, "claim.name": claimName, "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "multivalued": "false"}
	if claim.MultiValued {
		config["multivalued"] = "true"
		config["jsonType.label"] = "JSON"
	}
	return config
}

func keycloakMapperConfigMatches(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}
