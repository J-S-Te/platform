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
	adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer string
	httpClient                                                                              *stdhttp.Client
}

type keycloakClientResult struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"-"`
}

func newKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer string) *keycloakControlPlane {
	return &keycloakControlPlane{adminURL: strings.TrimRight(adminURL, "/"), realm: realm, username: username, password: password, brokerClientID: brokerClientID, brokerClientSecret: brokerClientSecret, platformIssuer: strings.TrimRight(platformIssuer, "/"), httpClient: &stdhttp.Client{Timeout: 12 * time.Second}}
}

// NewKeycloakControlPlane intentionally returns an opaque adapter.  Only the
// bootstrap package is allowed to construct it from process secrets.
func NewKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer string) *keycloakControlPlane {
	return newKeycloakControlPlane(adminURL, realm, username, password, brokerClientID, brokerClientSecret, platformIssuer)
}

func (control *keycloakControlPlane) token(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {control.username}, "password": {control.password}}
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
	existing := make(map[string]struct{}, len(attributes))
	for _, raw := range attributes {
		if attribute, ok := raw.(map[string]any); ok {
			if name, ok := attribute["name"].(string); ok {
				existing[name] = struct{}{}
			}
		}
	}
	for _, claim := range []string{"tenant_id", "person_id", "roles", "permissions", "role_config_hash", "authz_revision"} {
		if _, ok := existing[claim]; ok {
			continue
		}
		attributes = append(attributes, map[string]any{
			"name":        claim,
			"displayName": claim,
			"multivalued": claim == "roles" || claim == "permissions",
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
	payload := map[string]any{"alias": "basic-platform", "displayName": "基础平台", "providerId": "oidc", "enabled": true, "trustEmail": true, "storeToken": false, "config": map[string]string{"clientId": clientID, "clientSecret": clientSecret, "authorizationUrl": control.platformIssuer + "/authorize", "tokenUrl": control.platformIssuer + "/oauth2/token", "userInfoUrl": control.platformIssuer + "/oauth2/userinfo", "logoutUrl": control.platformIssuer + "/oauth2/logout", "defaultScope": "openid profile"}}
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
	for _, claim := range []string{"tenant_id", "person_id", "roles", "permissions", "role_config_hash", "authz_revision"} {
		mapper := map[string]any{"name": "platform-" + claim, "identityProviderAlias": "basic-platform", "identityProviderMapper": "oidc-user-attribute-idp-mapper", "config": map[string]string{"syncMode": "INHERIT", "claim": claim, "user.attribute": claim}}
		existing := mapperIDs["platform-"+claim]
		mapperID := existing.ID
		if mapperID != "" && existing.Config["syncMode"] == "INHERIT" && existing.Config["claim"] == claim && existing.Config["user.attribute"] == claim {
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
				return fmt.Errorf("delete Keycloak broker claim %s returned %d", claim, response.StatusCode)
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
		existing = mapperIDs["platform-"+claim]
		mapperID = existing.ID
		if mapperID == "" {
			return fmt.Errorf("configure Keycloak broker claim %s returned %d", claim, response.StatusCode)
		}
		if existing.Config["syncMode"] != "INHERIT" || existing.Config["claim"] != claim || existing.Config["user.attribute"] != claim {
			return fmt.Errorf("configure Keycloak broker claim %s returned %d", claim, response.StatusCode)
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

func (control *keycloakControlPlane) ensureClaimMappers(ctx context.Context, token, clientID string) error {
	base := "/admin/realms/" + url.PathEscape(control.realm) + "/clients/" + url.PathEscape(clientID) + "/protocol-mappers/models"
	response, err := control.request(ctx, token, stdhttp.MethodGet, base, nil)
	if err != nil {
		return err
	}
	var existing []struct {
		Name string `json:"name"`
	}
	err = json.NewDecoder(response.Body).Decode(&existing)
	response.Body.Close()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, mapper := range existing {
		known[mapper.Name] = true
	}
	claims := []string{"tenant_id", "person_id", "roles", "permissions", "role_config_hash", "authz_revision"}
	for _, claim := range claims {
		name := "platform-" + claim
		if known[name] {
			continue
		}
		config := map[string]string{"user.attribute": claim, "claim.name": claim, "jsonType.label": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "multivalued": "false"}
		if claim == "roles" || claim == "permissions" {
			config["multivalued"] = "true"
			config["jsonType.label"] = "JSON"
		}
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": name, "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper", "config": config})
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != stdhttp.StatusCreated {
			return fmt.Errorf("create Keycloak claim mapper %s returned %d", claim, response.StatusCode)
		}
	}
	if !known["platform-token-use"] {
		response, err = control.request(ctx, token, stdhttp.MethodPost, base, map[string]any{"name": "platform-token-use", "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper", "config": map[string]string{"claim.name": "token_use", "claim.value": "id_token", "claim.value.type": "String", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true"}})
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
