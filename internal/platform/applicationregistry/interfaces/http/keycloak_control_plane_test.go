package http

import (
	"context"
	"encoding/json"
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
