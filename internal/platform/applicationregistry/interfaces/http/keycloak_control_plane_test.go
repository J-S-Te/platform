package http

import (
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestKeycloakIdentityClaimMappingsKeepTokenSmallAndUseIdentityAsSubject(t *testing.T) {
	got := make([]string, 0, len(keycloakIdentityClaimMappings))
	for _, mapping := range keycloakIdentityClaimMappings {
		got = append(got, mapping.Name)
	}
	want := []string{"identity_id", "tenant_id", "person_id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapped claims = %#v, want %#v", got, want)
	}
	for _, mapping := range keycloakIdentityClaimMappings {
		if mapping.Name == "identity_id" && mapping.ClaimName != "sub" {
			t.Fatalf("identity_id claim name = %q, want sub", mapping.ClaimName)
		}
		if mapping.Name == "permissions" || mapping.Name == "organization_ids" || mapping.Name == "role_config_hash" || mapping.Name == "authz_revision" {
			t.Fatalf("detailed authorization claim %q must not be projected to Keycloak tokens", mapping.Name)
		}
	}
	identityMapper := keycloakIdentityClaimMapperConfig(keycloakIdentityClaimMapping{Name: "identity_id", ClaimName: "sub"})
	if identityMapper["user.attribute"] != "identity_id" || identityMapper["claim.name"] != "sub" {
		t.Fatalf("identity mapper config = %#v", identityMapper)
	}
}

func TestKeycloakAuthorizationProtocolMappersSplitTokenPurposeAndAudience(t *testing.T) {
	mappers := keycloakAuthorizationProtocolMappers("contract_management-prod-web")
	if len(mappers) != 3 {
		t.Fatalf("mapper count = %d, want 3", len(mappers))
	}
	byName := make(map[string]keycloakManagedProtocolMapper, len(mappers))
	for _, mapper := range mappers {
		byName[mapper.Name] = mapper
	}
	idToken := byName["platform-token-use-id"]
	if idToken.ProtocolMapper != "oidc-hardcoded-claim-mapper" || idToken.Config["claim.value"] != "id_token" || idToken.Config["id.token.claim"] != "true" || idToken.Config["access.token.claim"] != "false" {
		t.Fatalf("ID token mapper = %#v", idToken)
	}
	accessToken := byName["platform-token-use-access"]
	if accessToken.ProtocolMapper != "oidc-hardcoded-claim-mapper" || accessToken.Config["claim.value"] != "access_token" || accessToken.Config["id.token.claim"] != "false" || accessToken.Config["access.token.claim"] != "true" {
		t.Fatalf("access token mapper = %#v", accessToken)
	}
	audience := byName["platform-client-audience"]
	if audience.ProtocolMapper != "oidc-audience-mapper" || audience.Config["included.custom.audience"] != "contract_management-prod-web" || audience.Config["id.token.claim"] != "false" || audience.Config["access.token.claim"] != "true" {
		t.Fatalf("audience mapper = %#v", audience)
	}
}

func TestEnsureClaimMappersMigratesOnlyOwnedAndHistoricalAuthorizationMappers(t *testing.T) {
	type mapperDocument struct {
		ID             string            `json:"id"`
		Name           string            `json:"name"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	existing := make([]mapperDocument, 0, len(keycloakIdentityClaimMappings)+4)
	for _, claim := range keycloakIdentityClaimMappings {
		existing = append(existing, mapperDocument{
			ID: "id-" + claim.Name, Name: "platform-" + claim.Name,
			ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: keycloakIdentityClaimMapperConfig(claim),
		})
	}
	existing = append(existing,
		mapperDocument{ID: "id-old-token-use", Name: "platform-token-use", ProtocolMapper: "oidc-hardcoded-claim-mapper", Config: map[string]string{"claim.name": "token_use"}},
		mapperDocument{ID: "id-manual-permissions", Name: "manual-business-authorization", ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: map[string]string{"claim.name": "permissions"}},
		mapperDocument{ID: "id-stale-platform", Name: "platform-organization-ids", ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: map[string]string{"claim.name": "organization_ids"}},
		mapperDocument{ID: "id-third-party", Name: "third-party-profile", ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: map[string]string{"claim.name": "department"}},
	)
	deleted := map[string]bool{}
	created := map[string]mapperDocument{}
	basePath := "/admin/realms/platform/clients/internal-client-id/protocol-mappers/models"
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method {
		case stdhttp.MethodGet:
			if request.URL.Path != basePath {
				t.Fatalf("GET path = %q", request.URL.Path)
			}
			_ = json.NewEncoder(response).Encode(existing)
		case stdhttp.MethodDelete:
			deleted[strings.TrimPrefix(request.URL.Path, basePath+"/")] = true
			response.WriteHeader(stdhttp.StatusNoContent)
		case stdhttp.MethodPost:
			if request.URL.Path != basePath {
				t.Fatalf("POST path = %q", request.URL.Path)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var mapper mapperDocument
			if err := json.Unmarshal(body, &mapper); err != nil {
				t.Fatalf("decode mapper: %v; body=%s", err, body)
			}
			created[mapper.Name] = mapper
			response.WriteHeader(stdhttp.StatusCreated)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "", "", "", "", "", "")
	if err := control.ensureClaimMappers(context.Background(), "admin-token", "internal-client-id", "contract_management-prod-web"); err != nil {
		t.Fatalf("ensureClaimMappers() error = %v", err)
	}
	for _, id := range []string{"id-old-token-use", "id-manual-permissions", "id-stale-platform"} {
		if !deleted[id] {
			t.Errorf("mapper %q was not deleted", id)
		}
	}
	if deleted["id-third-party"] {
		t.Fatal("third-party profile mapper was deleted")
	}
	for _, name := range []string{"platform-token-use-id", "platform-token-use-access", "platform-client-audience"} {
		if _, ok := created[name]; !ok {
			t.Errorf("mapper %q was not created", name)
		}
	}
	if len(created) != 3 {
		t.Fatalf("created mappers = %#v, want only three authorization mappers", created)
	}
}

func TestKeycloakControlPlaneUsesInjectedServiceAccountCredentials(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.URL.Path != "/realms/master/protocol/openid-connect/token" {
			t.Fatalf("token path = %q", request.URL.Path)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if got, want := request.Form, (url.Values{"grant_type": {"client_credentials"}, "client_id": {"control-plane"}, "client_secret": {"service-account-secret"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("token form = %#v, want %#v", got, want)
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"access_token": "service-account-token"})
	}))
	defer server.Close()

	control := NewKeycloakControlPlaneWithCredentials(server.URL, "platform", KeycloakControlPlaneCredentials{
		ServiceAccountClientID: "control-plane", ServiceAccountClientSecret: "service-account-secret",
		Username: "legacy-admin", Password: "legacy-password",
	}, "broker", "broker-secret", "https://platform.example.com", "http://platform-api:8080")
	token, err := control.token(context.Background())
	if err != nil {
		t.Fatalf("get service-account token: %v", err)
	}
	if token != "service-account-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestKeycloakControlPlaneRetainsUsernamePasswordAuthentication(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if got, want := request.Form, (url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"secret"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("token form = %#v, want %#v", got, want)
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"access_token": "password-token"})
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "admin", "secret", "broker", "broker-secret", "https://platform.example.com", "http://platform-api:8080")
	token, err := control.token(context.Background())
	if err != nil {
		t.Fatalf("get password token: %v", err)
	}
	if token != "password-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestEnsureRealmCreatesRealmWithAuditEventRetention(t *testing.T) {
	t.Parallel()
	createCalled := false
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method {
		case stdhttp.MethodGet:
			if request.URL.Path != "/admin/realms/platform" {
				t.Fatalf("GET path = %q", request.URL.Path)
			}
			response.WriteHeader(stdhttp.StatusNotFound)
		case stdhttp.MethodPost:
			if request.URL.Path != "/admin/realms" {
				t.Fatalf("POST path = %q", request.URL.Path)
			}
			createCalled = true
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			expected := map[string]any{"realm": "platform", "enabled": true, "eventsEnabled": true, "adminEventsEnabled": true, "eventsExpiration": float64(keycloakDefaultRealmEventsExpirationSeconds)}
			for key, value := range expected {
				if !reflect.DeepEqual(payload[key], value) {
					t.Fatalf("realm payload %q = %#v, want %#v", key, payload[key], value)
				}
			}
			response.WriteHeader(stdhttp.StatusCreated)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "", "", "", "", "", "")
	if err := control.ensureRealm(context.Background(), "admin-token"); err != nil {
		t.Fatalf("ensureRealm() error = %v", err)
	}
	if !createCalled {
		t.Fatal("realm was not created")
	}
}

func TestEnsureRealmEnablesEventsAndAdminEvents(t *testing.T) {
	t.Parallel()
	gotPutPayload := false
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method {
		case stdhttp.MethodGet:
			if request.URL.Path != "/admin/realms/platform" {
				t.Fatalf("GET path = %q", request.URL.Path)
			}
			existing := map[string]any{"realm": "platform", "enabled": true, "eventsEnabled": false, "adminEventsEnabled": false, "eventsExpiration": float64(0)}
			_ = json.NewEncoder(response).Encode(existing)
		case stdhttp.MethodPut:
			if request.URL.Path != "/admin/realms/platform" {
				t.Fatalf("PUT path = %q", request.URL.Path)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if got, ok := payload["eventsEnabled"].(bool); !ok || !got {
				t.Fatalf("eventsEnabled = %#v", payload["eventsEnabled"])
			}
			if got, ok := payload["adminEventsEnabled"].(bool); !ok || !got {
				t.Fatalf("adminEventsEnabled = %#v", payload["adminEventsEnabled"])
			}
			expiration, ok := payload["eventsExpiration"].(float64)
			if !ok || int64(expiration) != keycloakDefaultRealmEventsExpirationSeconds {
				t.Fatalf("eventsExpiration = %#v, want %d", payload["eventsExpiration"], keycloakDefaultRealmEventsExpirationSeconds)
			}
			gotPutPayload = true
			response.WriteHeader(stdhttp.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "", "", "", "", "", "")
	if err := control.ensureRealm(context.Background(), "admin-token"); err != nil {
		t.Fatalf("ensureRealm() error = %v", err)
	}
	if !gotPutPayload {
		t.Fatal("realm audit payload was not updated")
	}
}

func TestKeycloakControlPlaneTokenFailureIncludesStatusAndRedactsResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.URL.Path != "/realms/master/protocol/openid-connect/token" {
			t.Fatalf("token path = %q", request.URL.Path)
		}
		response.WriteHeader(stdhttp.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"error":"invalid_client","client_secret":"must-not-leak","detail":"credentials rejected"}`))
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "admin", "secret", "broker", "broker-secret", "https://platform.example.com", "")
	_, err := control.token(context.Background())
	if err == nil {
		t.Fatal("token error = nil")
	}
	message := err.Error()
	for _, expected := range []string{"keycloak admin authentication", "status 401", `"client_secret":<redacted>`, "credentials rejected"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("token error = %q, missing %q", message, expected)
		}
	}
	if strings.Contains(message, "must-not-leak") {
		t.Fatalf("token error leaked secret: %q", message)
	}
}

func TestEnsureClientRejectsErrorStatusBeforeDecodingClientList(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(response stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.URL.Path {
		case "/realms/master/protocol/openid-connect/token":
			_ = json.NewEncoder(response).Encode(map[string]string{"access_token": "admin-token"})
		case "/admin/realms/platform":
			response.WriteHeader(stdhttp.StatusOK)
		case "/admin/realms/platform/clients":
			if request.URL.Query().Get("clientId") != "contract-web" {
				t.Fatalf("client lookup query = %q", request.URL.RawQuery)
			}
			response.WriteHeader(stdhttp.StatusForbidden)
			_, _ = response.Write([]byte(`{"error":"insufficient_scope","authorization":"Bearer must-not-leak"}`))
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()

	control := NewKeycloakControlPlane(server.URL, "platform", "admin", "secret", "broker", "broker-secret", "https://platform.example.com", "")
	_, err := control.EnsureClient(context.Background(), "contract-web", "合同管理", "https://contract.example.com/auth/callback")
	if err == nil {
		t.Fatal("EnsureClient error = nil")
	}
	message := err.Error()
	for _, expected := range []string{"read Keycloak Client", "status 403", "insufficient_scope", "<redacted>"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("EnsureClient error = %q, missing %q", message, expected)
		}
	}
	if strings.Contains(message, "must-not-leak") {
		t.Fatalf("EnsureClient error leaked authorization value: %q", message)
	}
}

func TestKeycloakResponseSummaryIsBounded(t *testing.T) {
	t.Parallel()
	summary := keycloakResponseSummary(strings.NewReader(strings.Repeat("x", keycloakAdminErrorSummaryLimit+32)))
	if !strings.HasSuffix(summary, "…") {
		t.Fatalf("summary = %q, want truncation marker", summary)
	}
	if len([]rune(summary)) != keycloakAdminErrorSummaryLimit+1 {
		t.Fatalf("summary length = %d, want %d", len([]rune(summary)), keycloakAdminErrorSummaryLimit+1)
	}
}

func TestKeycloakResponseSummaryRedactsPlainTextCredentials(t *testing.T) {
	t.Parallel()

	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.signature-value"
	for _, test := range []struct {
		name    string
		payload string
		secrets []string
	}{
		{
			name:    "authorization bearer token",
			payload: "upstream rejected request: Authorization: Bearer bearer-secret-must-not-leak",
			secrets: []string{"bearer-secret-must-not-leak"},
		},
		{
			name:    "cookie and set cookie",
			payload: "Cookie: KEYCLOAK_SESSION=session-secret-must-not-leak; AUTH_SESSION_ID=another-secret Set-Cookie: KC_RESTART=restart-secret-must-not-leak; Path=/",
			secrets: []string{"session-secret-must-not-leak", "another-secret", "restart-secret-must-not-leak"},
		},
		{
			name:    "jwt text",
			payload: "proxy response contains " + jwt,
			secrets: []string{jwt},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			summary := keycloakResponseSummary(strings.NewReader(test.payload))
			for _, secret := range test.secrets {
				if strings.Contains(summary, secret) {
					t.Fatalf("summary leaked credential %q: %q", secret, summary)
				}
			}
			if !strings.Contains(summary, "<redacted>") {
				t.Fatalf("summary = %q, want redaction", summary)
			}
		})
	}
}
