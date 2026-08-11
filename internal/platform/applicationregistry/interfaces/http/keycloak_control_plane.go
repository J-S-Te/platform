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
	"strings"
	"time"
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
	{Name: "identity_id"},
	{Name: "tenant_id"},
	{Name: "person_id"},
	{Name: "primary_org_id"},
	{Name: "organization_ids", MultiValued: true},
	{Name: "roles", MultiValued: true},
	{Name: "permissions", MultiValued: true},
	{Name: "role_config_hash"},
	{Name: "authz_revision"},
}

type keycloakIdentityClaimMapping struct {
	Name        string
	MultiValued bool
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
	if response.StatusCode != stdhttp.StatusOK {
		return "", fmt.Errorf("keycloak admin authentication returned %d", response.StatusCode)
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

func (control *keycloakControlPlane) ensureRealm(ctx context.Context, token string) error {
	response, err := control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm), nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode == stdhttp.StatusOK {
		return nil
	}
	if response.StatusCode != stdhttp.StatusNotFound {
		return fmt.Errorf("read Keycloak Realm returned %d", response.StatusCode)
	}
	response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms", map[string]any{"realm": control.realm, "enabled": true})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != stdhttp.StatusCreated && response.StatusCode != stdhttp.StatusConflict {
		return fmt.Errorf("create Keycloak Realm returned %d", response.StatusCode)
	}
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
	if response.StatusCode != stdhttp.StatusOK {
		return fmt.Errorf("read Keycloak user profile returned %d", response.StatusCode)
	}
	var profile map[string]any
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		return err
	}
	attributes, _ := profile["attributes"].([]any)
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
	if response.StatusCode != stdhttp.StatusOK && response.StatusCode != stdhttp.StatusNoContent {
		return fmt.Errorf("configure Keycloak user profile returned %d", response.StatusCode)
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
	response.Body.Close()
	backchannel := control.platformBackchannel
	if backchannel == "" {
		backchannel = control.platformIssuer
	}
	// The browser-facing endpoints must use the public issuer. Only the
	// token and userinfo calls are server-to-server and may use the backchannel;
	// exposing an internal Docker hostname as logoutUrl makes Keycloak redirect
	// the user's browser to an unreachable host during Broker logout.
	payload := map[string]any{"alias": "basic-platform", "displayName": "基础平台", "providerId": "oidc", "enabled": true, "trustEmail": true, "storeToken": false, "config": map[string]string{"clientId": clientID, "clientSecret": clientSecret, "clientAuthMethod": "client_secret_basic", "authorizationUrl": control.platformIssuer + "/authorize", "tokenUrl": backchannel + "/oauth2/token", "userInfoUrl": backchannel + "/oauth2/userinfo", "logoutUrl": control.platformIssuer + "/oauth2/logout", "defaultScope": "openid profile"}}
	if exists {
		response, err = control.request(ctx, token, stdhttp.MethodPut, base, payload)
	} else {
		response, err = control.request(ctx, token, stdhttp.MethodPost, "/admin/realms/"+url.PathEscape(control.realm)+"/identity-provider/instances", payload)
	}
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent && response.StatusCode != stdhttp.StatusCreated {
		return fmt.Errorf("configure Keycloak broker returned %d", response.StatusCode)
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
		if response.StatusCode != stdhttp.StatusOK {
			return nil, fmt.Errorf("read Keycloak broker mappers returned %d", response.StatusCode)
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
			response.Body.Close()
			if response.StatusCode != stdhttp.StatusNoContent && response.StatusCode != stdhttp.StatusNotFound {
				return fmt.Errorf("delete Keycloak broker claim %s returned %d", claim.Name, response.StatusCode)
			}
		}
		response, err = control.request(ctx, token, method, path, mapper)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode == stdhttp.StatusCreated {
			continue
		}
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
			return fmt.Errorf("configure Keycloak broker claim %s returned %d", claim.Name, response.StatusCode)
		}
		if existing.Config["syncMode"] != "INHERIT" || existing.Config["claim"] != claim.Name || existing.Config["user.attribute"] != claim.Name {
			return fmt.Errorf("configure Keycloak broker claim %s returned %d", claim.Name, response.StatusCode)
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
		response.Body.Close()
		if response.StatusCode != stdhttp.StatusCreated {
			return keycloakClientResult{}, fmt.Errorf("create Keycloak Client returned %d", response.StatusCode)
		}
		response, err = control.request(ctx, token, stdhttp.MethodGet, path, nil)
		if err != nil {
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
	response.Body.Close()
	if response.StatusCode != stdhttp.StatusNoContent {
		return keycloakClientResult{}, fmt.Errorf("update Keycloak Client returned %d", response.StatusCode)
	}
	if err := control.ensureClaimMappers(ctx, token, internalID); err != nil {
		return keycloakClientResult{}, err
	}
	response, err = control.request(ctx, token, stdhttp.MethodGet, "/admin/realms/"+url.PathEscape(control.realm)+"/clients/"+url.PathEscape(internalID)+"/client-secret", nil)
	if err != nil {
		return keycloakClientResult{}, err
	}
	defer response.Body.Close()
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
	if response.StatusCode != stdhttp.StatusOK {
		return fmt.Errorf("list Keycloak Client roles returned %d", response.StatusCode)
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
		created.Body.Close()
		if created.StatusCode != stdhttp.StatusCreated && created.StatusCode != stdhttp.StatusConflict {
			return fmt.Errorf("create Keycloak Client role %s returned %d", code, created.StatusCode)
		}
	}
	return nil
}

func (control *keycloakControlPlane) ensureClaimMappers(ctx context.Context, token, clientID string) error {
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clientID) + "/protocol-mappers/models"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
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
	for _, claim := range keycloakIdentityClaimMappings {
		name := "platform-" + claim.Name
		config := map[string]string{"user.attribute": claim.Name, "claim.name": claim.Name, "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "multivalued": "false"}
		if claim.MultiValued {
			config["multivalued"] = "true"
			config["jsonType.label"] = "JSON"
		}
		if current, exists := known[name]; exists {
			if current.ProtocolMapper == "oidc-usermodel-attribute-mapper" && keycloakMapperConfigMatches(current.Config, config) {
				continue
			}
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
			if err != nil {
				return err
			}
			response.Body.Close()
			if response.StatusCode != stdhttp.StatusNoContent && response.StatusCode != stdhttp.StatusNotFound {
				return fmt.Errorf("delete drifted Keycloak claim mapper %s returned %d", claim.Name, response.StatusCode)
			}
		}
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": name, "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper", "config": config})
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != stdhttp.StatusCreated {
			return fmt.Errorf("create Keycloak claim mapper %s returned %d", claim.Name, response.StatusCode)
		}
	}
	tokenUseConfig := map[string]string{"claim.name": "token_use", "claim.value": "id_token", "claim.value.type": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"}
	if current, exists := known["platform-token-use"]; !exists || current.ProtocolMapper != "oidc-hardcoded-claim-mapper" || !keycloakMapperConfigMatches(current.Config, tokenUseConfig) {
		if exists {
			response, err = control.request(ctx, token, stdhttp.MethodDelete, base+"/"+url.PathEscape(current.ID), nil)
			if err != nil {
				return err
			}
			response.Body.Close()
			if response.StatusCode != stdhttp.StatusNoContent && response.StatusCode != stdhttp.StatusNotFound {
				return fmt.Errorf("delete drifted Keycloak token_use mapper returned %d", response.StatusCode)
			}
		}
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": "platform-token-use", "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper", "config": tokenUseConfig})
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != stdhttp.StatusCreated {
			return fmt.Errorf("create Keycloak token_use mapper returned %d", response.StatusCode)
		}
	}
	return nil
}

func keycloakMapperConfigMatches(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}
