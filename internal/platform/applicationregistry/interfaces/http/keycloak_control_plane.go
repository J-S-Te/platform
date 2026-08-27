package http

// This adapter owns the small, auditable subset of the Keycloak Admin API used
// by application onboarding.  It deliberately does not proxy the Admin API to
// browsers and never returns an access token or a client secret.

import (
	"bytes"
	"context"
	"encoding/json"
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

// KeycloakControlPlaneCredentials explicitly supplies the credentials used by
// the control plane to call the Keycloak Admin API. When a complete service
// account pair is supplied it is preferred and exchanged with
// client_credentials. Username/password remains supported for existing
// deployments and is used when no service account is configured.
//
// The credentials are process-only inputs; the adapter never returns them.
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

// keycloakIdentityClaimMappings is the explicit, one-way projection from the
// Basic Platform identity/authorization model into a brokered Keycloak user.
// Keycloak may relay these values to relying parties, but it is not allowed to
// become the source of truth for personnel, organisation or authorization.
var keycloakIdentityClaimMappings = []keycloakIdentityClaimMapping{
	// identity_id is the immutable platform principal. Publish it as the OIDC
	// subject so every relying party uses one stable identity key.
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

// NewKeycloakControlPlane intentionally returns an opaque adapter.  Only the
// bootstrap package is allowed to construct it from process secrets.
func NewKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel string) *keycloakControlPlane {
	return newKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer, platformBackchannel)
}

// NewKeycloakControlPlaneWithCredentials constructs a control plane with
// explicitly injected Admin API credentials. A complete service-account pair
// uses client_credentials; otherwise the supplied username/password pair is
// used for backwards compatibility.
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

// keycloakStatusError produces a safe, bounded diagnostic for an Admin API
// failure. It must be called before the response body is closed. The response
// body is intentionally not decoded as a successful Keycloak payload when its
// status is unexpected.
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
	// Keycloak usually returns JSON, but reverse proxies and custom extensions
	// can return plain-text HTTP headers. Redact those forms before the generic
	// key/value matcher so a value such as "Authorization: Bearer <token>"
	// cannot leave the token following the word "Bearer" in logs.
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
		// A compatible Admin API proxy may answer 200 without returning the
		// Realm representation. Keep onboarding compatible; reconciliation
		// will enforce the settings once a complete response is available.
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

// ensurePrelinkedBrokerFlow creates a provider-specific, fail-closed first
// login flow. Keycloak skips this flow for an already linked Federated
// Identity. An unknown subject reaches the single deny execution instead of
// entering Keycloak's JIT user/profile registration flow.
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

// ensureBrokerReviewProfileDisabled reconciles the effective Keycloak 26
// setting. updateProfileFirstLoginMode on the IdentityProvider representation
// is retained for compatibility, but current Keycloak versions read the mode
// from the Review Profile authenticator execution in the first Broker flow.
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
	// A custom flow without Review Profile already satisfies the no-interaction
	// contract and needs no authenticator configuration.
	return nil
}

// ensureBrokerUserProfile declares the attributes imported from Basic Platform.
// Keycloak 26 validates mapper targets against the Realm user-profile schema,
// so an Attribute Importer cannot create arbitrary attributes on first login.
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
	// Basic Platform owns profile validation. Keycloak must not turn optional
	// platform fields into a first-login registration form.
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
			// Repair any manually changed profile metadata during every sync so
			// a Keycloak administrator cannot silently turn a list claim into a
			// scalar or make the mapping user-editable.
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

// EnsureBroker registers an upstream Basic Platform identity provider and
// imports the authorization claims into Keycloak user attributes. The alias is
// intentionally explicit: internal employees and external customers use
// different Broker clients so one application's callback cannot overwrite the
// other's client credentials.
func (control *keycloakControlPlane) EnsureBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform", "基础平台", clientID, clientSecret)
}

// EnsureCustomerPortalBroker configures the isolated customer portal IdP.
func (control *keycloakControlPlane) EnsureCustomerPortalBroker(ctx context.Context, clientID, clientSecret string) error {
	return control.ensureBroker(ctx, "basic-platform-customer", "客户自助门户", clientID, clientSecret)
}

// VerifyBrokerExists checks that the basic-platform IdP exists and has a
// complete config. It does not modify the IdP or require a client secret.
// Use this when the platform already has active credentials and only needs
// to confirm the Keycloak side is healthy.
func (control *keycloakControlPlane) VerifyBrokerExists(ctx context.Context) error {
	return control.verifyBrokerExists(ctx, "basic-platform")
}

// VerifyCustomerPortalBrokerExists checks the customer portal IdP health.
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
		return fmt.Errorf("Keycloak IdP %s does not exist", brokerAlias)
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
			return fmt.Errorf("Keycloak IdP %s config incomplete: missing %s", brokerAlias, key)
		}
	}
	return nil
}

// KeycloakDriftReport describes detected configuration differences between the
// platform's expected state and the actual Keycloak state for one subsystem client.
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

// DetectSubsystemKeycloakDrift compares the expected Keycloak configuration for a
// subsystem client against the actual state. It checks:
//   - Client existence and redirect URI
//   - Role catalog completeness
//   - Protocol mapper presence and configuration
//   - Broker IdP config completeness
//
// This method is read-only and does not modify Keycloak state. Use the result
// to decide whether to trigger a reconciliation.
func (control *keycloakControlPlane) DetectSubsystemKeycloakDrift(ctx context.Context, clientID, expectedRedirectURI string, expectedRoleCodes []string) (KeycloakDriftReport, error) {
	report := KeycloakDriftReport{ClientID: clientID, RedirectURIOK: true, BrokerConfigOK: true}
	token, err := control.token(ctx)
	if err != nil {
		return report, err
	}
	// Check client exists.
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
	// Check redirect URI.
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
	// Check roles.
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
	// Compare the platform-owned protocol mappers by name and configuration;
	// a mapper with the right name but a changed claim is still drift.
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
	// Check broker config.
	brokerAlias := brokerAliasForClient(clientID)
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances/"+url.PathEscape(brokerAlias), nil)
	if err == nil {
		if response.StatusCode == stdhttp.StatusOK {
			var broker struct {
				Config map[string]string `json:"config"`
			}
			decodeBrokerErr := json.NewDecoder(response.Body).Decode(&broker)
			response.Body.Close()
			if decodeBrokerErr == nil {
				requiredKeys := []string{"clientId", "clientSecret", "tokenUrl", "userInfoUrl", "authorizationUrl"}
				for _, key := range requiredKeys {
					if strings.TrimSpace(broker.Config[key]) == "" {
						report.BrokerConfigOK = false
						report.HasDrift = true
						break
					}
				}
			}
		} else {
			response.Body.Close()
			report.BrokerConfigOK = false
			report.HasDrift = true
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
	// The browser-facing endpoints must use the public issuer. Only the
	// token and userinfo calls are server-to-server and may use the backchannel;
	// exposing an internal Docker hostname as logoutUrl makes Keycloak redirect
	// the user's browser to an unreachable host during Broker logout.
	// Users are provisioned and linked by the platform authorization projection
	// before they can enter a subsystem. Never expose Keycloak's first-login
	// profile completion form: email and split first/last names are optional in
	// the platform and must not become an additional registration contract.
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
	// Self-healing: verify IdP config completeness. Keycloak can return an
	// empty config{} after a timing race or cache miss during PUT. Re-apply
	// the payload once; if it still fails, surface the error so the worker
	// retries on next startup instead of silently leaving a broken IdP.
	if err := control.ensureBrokerConfigComplete(ctx, token, base, brokerAlias, payload, verifiedBroker.Config); err != nil {
		return err
	}
	// Keycloak 26 can keep returning the legacy top-level profile mode as
	// "on". The provider-specific deny-only flow is the authoritative
	// no-registration contract.
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
			// Keycloak 26's cache layer can reject in-place mapper updates for
			// otherwise valid records. Delete and recreate only a drifted mapper.
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
		// Keycloak 26.2 can persist an IdP mapper then return 400 while it
		// finalizes the Admin event. Re-read and update it so the operation
		// remains correct and safe to retry instead of exposing a false 503.
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

// ensureBrokerConfigComplete verifies that the IdP config received from Keycloak
// contains all required fields. If the config is empty or missing critical keys,
// it re-applies the expected payload once. This self-heals the known Keycloak
// race condition where a PUT returns 204 but the config remains empty {}.
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
	// Re-apply the expected configuration once.
	response, err := control.request(ctx, token, stdhttp.MethodPut, base, expected)
	if err != nil {
		return fmt.Errorf("self-heal Keycloak IdP %s config: %w", brokerAlias, err)
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return fmt.Errorf("self-heal Keycloak IdP %s config returned HTTP %d", brokerAlias, response.StatusCode)
	}
	// Verify the fix took effect.
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
			return fmt.Errorf("Keycloak IdP %s config still incomplete after self-heal: missing %s", brokerAlias, key)
		}
	}
	return nil
}

// EnsureClient creates/updates a confidential RP and the claims expected by
// the existing subsystems. Attribute import from the upstream platform IdP is
// configured separately before this method is allowed to switch production.
func (control *keycloakControlPlane) EnsureClient(ctx context.Context, clientID, name, redirectURI string) (keycloakClientResult, error) {
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
	payload := map[string]any{"clientId": clientID, "name": name, "enabled": true, "protocol": "openid-connect", "publicClient": false, "standardFlowEnabled": true, "directAccessGrantsEnabled": false, "redirectUris": []string{redirectURI}, "webOrigins": []string{"+"}}
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

// EnsureClientRoles mirrors the platform-published role catalog into the
// target Keycloak Client. Roles remain namespaced by Client, so an "admin"
// role in contract management can never grant CRM or project access.
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
		// identity_id is managed by the explicit stable mapper below.  The legacy
		// generic projection named platform-identity_id used to map this attribute
		// to the reserved OIDC `sub` claim, which overwrote the canonical subject
		// and made Portal reject otherwise valid authorization-code logins.
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

	// Remove stale platform-owned mappers and exact historical detailed
	// authorization Claims. Matching claim.name instead of fuzzy mapper names
	// migrates manually named legacy mappers without touching third-party
	// profile or role mappers.
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
			// Keep the stable platform identity available under its explicit
			// contract name as well as OIDC sub. Existing relying parties use
			// identity_id, while sub remains the canonical OIDC subject.
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
