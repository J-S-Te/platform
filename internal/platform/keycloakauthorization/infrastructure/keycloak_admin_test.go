package infrastructure

import (
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
)

func TestKeycloakAdminProjectsGroupsRolesAndClientAttributes(t *testing.T) {
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "")
	var requests []string
	var tokenForms []url.Values
	var mappedRoles []map[string]any
	var updatedUsers []keycloakUser
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		requests = append(requests, request.Method+" "+request.URL.EscapedPath())
		if request.URL.Path == "/realms/master/protocol/openid-connect/token" {
			if err := request.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			tokenForms = append(tokenForms, request.PostForm)
			writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "admin-token"})
			return
		}
		if request.Header.Get("Authorization") != "Bearer admin-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users":
			if got := request.URL.Query().Get("q"); got != "identity_id:identity-1" {
				t.Errorf("user query q = %q", got)
			}
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{{
				ID: "user-1", Username: "platform-identity-1", Enabled: true,
				Attributes: map[string][]string{"identity_id": []string{"identity-1"}, "unmanaged": []string{"keep"}},
			}})
		case "PUT /admin/realms/acme/users/user-1":
			var user keycloakUser
			if err := json.NewDecoder(request.Body).Decode(&user); err != nil {
				t.Fatalf("decode user: %v", err)
			}
			updatedUsers = append(updatedUsers, user)
			writer.WriteHeader(stdhttp.StatusNoContent)
		case "GET /admin/realms/acme/groups":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]string{{"id": "root", "name": platformGroupsRoot}})
		case "GET /admin/realms/acme/groups/root/children":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]string{{"id": "tenant", "name": "tenant-tenant-1"}})
		case "GET /admin/realms/acme/groups/tenant/children":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]string{{"id": "org", "name": "organization-org-1"}})
		case "PUT /admin/realms/acme/users/user-1/groups/org":
			writer.WriteHeader(stdhttp.StatusNoContent)
		case "GET /admin/realms/acme/users/user-1/groups":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakGroup{})
		case "GET /admin/realms/acme/clients":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]string{{"id": "client-internal", "clientId": "orders"}})
		case "GET /admin/realms/acme/clients/client-internal/roles":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]string{})
		case "POST /admin/realms/acme/clients/client-internal/roles":
			writer.WriteHeader(stdhttp.StatusCreated)
		case "GET /admin/realms/acme/clients/client-internal/roles/manager":
			writeJSON(t, writer, stdhttp.StatusOK, map[string]any{"id": "role-1", "name": "manager", "clientRole": true, "containerId": "client-internal"})
		case "GET /admin/realms/acme/users/user-1/role-mappings/clients/client-internal":
			writeJSON(t, writer, stdhttp.StatusOK, []map[string]any{})
		case "POST /admin/realms/acme/users/user-1/role-mappings/clients/client-internal":
			if err := json.NewDecoder(request.Body).Decode(&mappedRoles); err != nil {
				t.Fatalf("decode role mapping: %v", err)
			}
			writer.WriteHeader(stdhttp.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
			writer.WriteHeader(stdhttp.StatusNotFound)
		}
	}))
	defer server.Close()

	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "secret", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	snapshot := projectionapplication.Snapshot{TenantID: "tenant-1", IdentityID: "identity-1", KeycloakClientID: "orders", UserEnabled: true, OrganizationIDs: []string{"org-1"}, Roles: []string{"manager"}, Permissions: []string{"orders:read"}, RoleConfigHash: "hash-1", AuthorizationRevision: 7}
	if err := admin.EnsureOrganizationGroups(context.Background(), snapshot); err != nil {
		t.Fatalf("EnsureOrganizationGroups: %v", err)
	}
	if err := admin.AssignClientRoles(context.Background(), snapshot); err != nil {
		t.Fatalf("AssignClientRoles: %v", err)
	}
	if err := admin.SetClientAuthorizationAttributes(context.Background(), snapshot); err != nil {
		t.Fatalf("SetClientAuthorizationAttributes: %v", err)
	}

	if len(tokenForms) != 3 {
		t.Fatalf("token requests = %d, want 3", len(tokenForms))
	}
	for _, form := range tokenForms {
		if form.Get("grant_type") != "password" || form.Get("client_id") != "admin-cli" || form.Get("username") != "admin" || form.Get("password") != "secret" {
			t.Errorf("unexpected token form: %v", form)
		}
	}
	if len(mappedRoles) != 1 || mappedRoles[0]["name"] != "manager" {
		t.Fatalf("role mapping = %#v", mappedRoles)
	}
	if len(updatedUsers) != 4 {
		t.Fatalf("user updates = %d, want 4", len(updatedUsers))
	}
	if !updatedUsers[len(updatedUsers)-1].Enabled {
		t.Error("enabled platform user was not re-enabled in Keycloak")
	}
	attributes := updatedUsers[len(updatedUsers)-1].Attributes
	if got := attributes["identity_id"]; len(got) != 1 || got[0] != "identity-1" {
		t.Errorf("identity_id = %#v", got)
	}
	if got := attributes["client_orders_permissions"]; len(got) != 1 || got[0] != "orders:read" {
		t.Errorf("client permission = %#v", got)
	}
	if got := attributes["client_orders_authz_revision"]; len(got) != 1 || got[0] != "7" {
		t.Errorf("client revision = %#v", got)
	}
	if got := attributes["unmanaged"]; len(got) != 1 || got[0] != "keep" {
		t.Errorf("unmanaged attribute was lost: %#v", got)
	}
	if !containsRequest(requests, "PUT /admin/realms/acme/users/user-1/groups/org") {
		t.Error("organization membership request was not sent")
	}
}

func TestKeycloakAdminReturnsTokenHTTPFailures(t *testing.T) {
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "")
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		writer.WriteHeader(stdhttp.StatusUnauthorized)
		_, _ = io.WriteString(writer, "invalid administrator credentials")
	}))
	defer server.Close()
	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "wrong", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	err = admin.EnsureUser(context.Background(), projectionapplication.Snapshot{IdentityID: "identity-1"})
	if err == nil || !strings.Contains(err.Error(), "request Keycloak admin token returned HTTP 401") {
		t.Fatalf("EnsureUser error = %v", err)
	}
}

func TestKeycloakAdminUsesServiceAccountClientCredentialsWhenConfigured(t *testing.T) {
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "platform-admin")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "client-secret")
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.URL.Path != "/realms/master/protocol/openid-connect/token" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
			writer.WriteHeader(stdhttp.StatusNotFound)
			return
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		form := request.PostForm
		if got, want := form.Get("grant_type"), "client_credentials"; got != want {
			t.Errorf("grant_type = %q, want %q", got, want)
		}
		if got, want := form.Get("client_id"), "platform-admin"; got != want {
			t.Errorf("client_id = %q, want %q", got, want)
		}
		if got, want := form.Get("client_secret"), "client-secret"; got != want {
			t.Errorf("client_secret = %q, want %q", got, want)
		}
		if form.Get("username") != "" || form.Get("password") != "" {
			t.Errorf("client credentials token form contains password grant fields: %v", form)
		}
		writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "service-token"})
	}))
	defer server.Close()

	admin, err := NewKeycloakAdmin(server.URL, "acme", "", "", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	if _, err := admin.token(context.Background()); err != nil {
		t.Fatalf("token() error = %v", err)
	}
}

func TestKeycloakAdminDisablesUserAndRevokesOnlyManagedOrganizationGroups(t *testing.T) {
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "")
	var updated keycloakUser
	var deletedGroups []string
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.URL.Path == "/realms/master/protocol/openid-connect/token" {
			writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "admin-token"})
			return
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{{ID: "user-1", Attributes: map[string][]string{"identity_id": {"identity-1"}}}})
		case "PUT /admin/realms/acme/users/user-1":
			if err := json.NewDecoder(request.Body).Decode(&updated); err != nil {
				t.Fatalf("decode user: %v", err)
			}
			writer.WriteHeader(stdhttp.StatusNoContent)
		case "GET /admin/realms/acme/users/user-1/groups":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakGroup{
				{ID: "old-managed", Path: "/basic-platform/tenant-tenant-1/organization-old-org"},
				{ID: "other-tenant", Path: "/basic-platform/tenant-tenant-2/organization-keep"},
				{ID: "external", Path: "/external/keep"},
			})
		case "DELETE /admin/realms/acme/users/user-1/groups/old-managed":
			deletedGroups = append(deletedGroups, "old-managed")
			writer.WriteHeader(stdhttp.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
			writer.WriteHeader(stdhttp.StatusNotFound)
		}
	}))
	defer server.Close()

	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "secret", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	snapshot := projectionapplication.Snapshot{TenantID: "tenant-1", IdentityID: "identity-1", UserEnabled: false}
	if err := admin.EnsureUser(context.Background(), snapshot); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if err := admin.EnsureOrganizationGroups(context.Background(), snapshot); err != nil {
		t.Fatalf("EnsureOrganizationGroups: %v", err)
	}
	if updated.Enabled {
		t.Error("disabled platform user remained enabled in Keycloak")
	}
	if len(deletedGroups) != 1 || deletedGroups[0] != "old-managed" {
		t.Fatalf("deleted groups = %#v", deletedGroups)
	}
}

func writeJSON(t *testing.T, writer stdhttp.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
}

func containsRequest(requests []string, wanted string) bool {
	for _, request := range requests {
		if request == wanted {
			return true
		}
	}
	return false
}
