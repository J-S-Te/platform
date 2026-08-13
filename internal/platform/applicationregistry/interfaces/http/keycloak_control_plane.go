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

const keycloakDefaultRealmEventsExpirationSeconds = int64(30 * 24 * 60 * 60)

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
		"eventsEnabled": true, "adminEventsEnabled": true, "eventsExpiration": keycloakDefaultRealmEventsExpirationSeconds,
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
	payload := make(map[string]any, 5)
	changed := false
	payload["registrationAllowed"] = false
	payload["verifyEmail"] = false
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
	if changed {
		return true, payload
	}
	return false, nil
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

func (control *keycloakControlPlane) brokerRepresentation(clientID, clientSecret, backchannel string) map[string]any {
	return map[string]any{
		"alias": "basic-platform", "displayName": "基础平台", "providerId": "oidc",
		"enabled": true, "trustEmail": true, "storeToken": false,
		"updateProfileFirstLoginMode": "off",
		"config": map[string]string{
			"clientId": clientID, "clientSecret": clientSecret, "clientAuthMethod": "client_secret_basic",
			"authorizationUrl": control.platformIssuer + "/authorize", "tokenUrl": backchannel + "/oauth2/token",
			"userInfoUrl": backchannel + "/oauth2/userinfo", "logoutUrl": control.platformIssuer + "/oauth2/logout",
			"defaultScope": "openid profile",
		},
	}
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
	return nil
}

// EnsureBroker registers Basic Platform as the Realm's only upstream identity
// provider and imports the authorization claims into Keycloak user attributes.
func (control *keycloakControlPlane) EnsureBroker(ctx context.Context, clientID, clientSecret string) error {
	token, err := control.token(ctx)
	if err != nil {
		return err
	}
	if err := control.ensureRealm(ctx, token); err != nil {
		return err
	}
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/identity-provider/instances/basic-platform"
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
	payload := control.brokerRepresentation(clientID, clientSecret, backchannel)
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
		mapper := map[string]any{"name": "platform-" + claim.Name, "identityProviderAlias": "basic-platform", "identityProviderMapper": "oidc-user-attribute-idp-mapper", "config": map[string]string{"syncMode": "INHERIT", "claim": claim.Name, "user.attribute": claim.Name}}
		existing := mapperIDs["platform-"+claim.Name]
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
		existing = mapperIDs["platform-"+claim.Name]
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
