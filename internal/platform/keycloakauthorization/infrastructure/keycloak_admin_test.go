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
				Attributes: map[string][]string{
					"identity_id": {"identity-1"}, "unmanaged": {"keep"}, "client_theme": {"dark"},
					"roles": {"legacy-role"}, "permissions": {"legacy.read"}, "organization_ids": {"old-org"},
					"role_config_hash": {"old-hash"}, "authz_revision": {"6"},
					"client_orders_roles": {"legacy-role"}, "client_orders_permissions": {"legacy.read"},
					"client_orders_organization_ids": {"old-org"}, "client_orders_authz_revision": {"6"},
					"client_orders_role_config_hash": {"old-hash"},
				},
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
	snapshot := projectionapplication.Snapshot{TenantID: "tenant-1", IdentityID: "identity-1", PersonID: "person-1", PrimaryOrganizationID: "org-1", KeycloakClientID: "orders", UserEnabled: true, OrganizationIDs: []string{"org-1"}, Roles: []string{"manager"}, Permissions: []string{"orders:read"}, RoleConfigHash: "hash-1", AuthorizationRevision: 7}
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
	if got := attributes["tenant_id"]; len(got) != 1 || got[0] != "tenant-1" {
		t.Errorf("tenant_id = %#v", got)
	}
	if got := attributes["person_id"]; len(got) != 1 || got[0] != "person-1" {
		t.Errorf("person_id = %#v", got)
	}
	if got := attributes["primary_org_id"]; len(got) != 1 || got[0] != "org-1" {
		t.Errorf("primary_org_id = %#v", got)
	}
	for _, key := range []string{
		"roles", "permissions", "organization_ids", "role_config_hash", "authz_revision",
		"client_orders_roles", "client_orders_permissions", "client_orders_organization_ids",
		"client_orders_authz_revision", "client_orders_role_config_hash",
	} {
		if _, exists := attributes[key]; exists {
			t.Errorf("managed authorization attribute %q was not removed: %#v", key, attributes[key])
		}
	}
	if got := attributes["unmanaged"]; len(got) != 1 || got[0] != "keep" {
		t.Errorf("unmanaged attribute was lost: %#v", got)
	}
	if got := attributes["client_theme"]; len(got) != 1 || got[0] != "dark" {
		t.Errorf("non-authorization client attribute was lost: %#v", got)
	}
	if !containsRequest(requests, "PUT /admin/realms/acme/users/user-1/groups/org") {
		t.Error("organization membership request was not sent")
	}
}

func TestLogoutIdentitySessionsRevokesBrokerAndProjectedUsersOnce(t *testing.T) {
	var logoutPaths []string
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /realms/master/protocol/openid-connect/token":
			writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "admin-token"})
		case "GET /admin/realms/acme/users":
			if request.URL.Query().Get("idpAlias") == platformBrokerAlias {
				writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{{ID: "user-1"}})
				return
			}
			if request.URL.Query().Get("q") != "identity_id:identity-1" {
				t.Errorf("unexpected user query: %s", request.URL.RawQuery)
			}
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{
				{ID: "user-1", Attributes: map[string][]string{"identity_id": {"identity-1"}}},
				{ID: "legacy-user", Attributes: map[string][]string{"identity_id": {"identity-1"}}},
			})
		case "POST /admin/realms/acme/users/user-1/logout", "POST /admin/realms/acme/users/legacy-user/logout":
			logoutPaths = append(logoutPaths, request.URL.Path)
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
	if err := admin.LogoutIdentitySessions(context.Background(), "identity-1"); err != nil {
		t.Fatalf("LogoutIdentitySessions() error = %v", err)
	}
	if len(logoutPaths) != 2 {
		t.Fatalf("logout requests = %v, want two unique users", logoutPaths)
	}
}

func TestLogoutIdentitySessionsTreatsMissingUserAsSuccess(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /realms/master/protocol/openid-connect/token":
			writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "admin-token"})
		case "GET /admin/realms/acme/users":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{})
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
	if err := admin.LogoutIdentitySessions(context.Background(), "identity-missing"); err != nil {
		t.Fatalf("LogoutIdentitySessions() error = %v", err)
	}
}

func TestManagedAuthorizationAttributeMatchingIsPrecise(t *testing.T) {
	for _, key := range []string{
		"permissions", "organization_ids", "role_config_hash", "authz_revision", "roles",
		"client_orders_permissions", "client_orders_organization_ids", "client_orders_role_config_hash",
		"client_orders_authz_revision", "client_orders_roles",
	} {
		if !isManagedAuthorizationAttribute(key) {
			t.Errorf("managed attribute %q was not recognized", key)
		}
	}
	for _, key := range []string{"identity_id", "tenant_id", "person_id", "primary_org_id", "client_theme", "unmanaged", "client_permissions_hint"} {
		if isManagedAuthorizationAttribute(key) {
			t.Errorf("unmanaged or stable attribute %q was incorrectly selected", key)
		}
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

func TestEnsureUserPrelinksBrokerIdentityAndAllowsOptionalProfileFields(t *testing.T) {
	var created map[string]any
	var linked keycloakFederatedIdentity
	var server *httptest.Server
	server = httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		if request.URL.Path == "/realms/master/protocol/openid-connect/token" {
			writeJSON(t, writer, stdhttp.StatusOK, map[string]string{"access_token": "admin-token"})
			return
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{})
		case "POST /admin/realms/acme/users":
			if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
				t.Fatalf("decode created user: %v", err)
			}
			writer.Header().Set("Location", server.URL+"/admin/realms/acme/users/user-1")
			writer.WriteHeader(stdhttp.StatusCreated)
		case "GET /admin/realms/acme/users/user-1/federated-identity":
			if linked.IdentityProvider == "" {
				writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{})
				return
			}
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{linked})
		case "POST /admin/realms/acme/users/user-1/federated-identity/basic-platform":
			if err := json.NewDecoder(request.Body).Decode(&linked); err != nil {
				t.Fatalf("decode Broker identity: %v", err)
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
	if err := admin.EnsureUser(context.Background(), projectionapplication.Snapshot{IdentityID: "identity-1", DisplayName: "测试用户", UserEnabled: true}); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if created["firstName"] != "测试用户" {
		t.Fatalf("created firstName = %#v", created["firstName"])
	}
	for _, optional := range []string{"email", "lastName", "requiredActions"} {
		if _, exists := created[optional]; exists {
			t.Fatalf("optional Keycloak field %q must be omitted: %#v", optional, created)
		}
	}
	if linked.IdentityProvider != platformBrokerAlias || linked.UserID != "identity-1" || linked.UserName != "platform-identity-1" {
		t.Fatalf("Broker identity = %#v", linked)
	}
}

func TestEnsureBrokerIdentityTreatsConcurrentSameLinkAsSuccess(t *testing.T) {
	readCount := 0
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users/user-1/federated-identity":
			readCount++
			if readCount == 1 {
				writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{})
				return
			}
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{{IdentityProvider: platformBrokerAlias, UserID: "identity-1"}})
		case "POST /admin/realms/acme/users/user-1/federated-identity/basic-platform":
			writer.WriteHeader(stdhttp.StatusConflict)
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "secret", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	if err := admin.ensureBrokerIdentity(context.Background(), "admin-token", keycloakUser{ID: "user-1"}, "identity-1"); err != nil {
		t.Fatalf("ensureBrokerIdentity() concurrent idempotency error = %v", err)
	}
}

func TestEnsureBrokerIdentityReportsConflictingOwnerAfter409(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users/user-1/federated-identity":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{})
		case "POST /admin/realms/acme/users/user-1/federated-identity/basic-platform":
			writer.WriteHeader(stdhttp.StatusConflict)
		case "GET /admin/realms/acme/users":
			if request.URL.Query().Get("idpAlias") != platformBrokerAlias || request.URL.Query().Get("idpUserId") != "identity-1" {
				t.Fatalf("Broker owner query = %s", request.URL.RawQuery)
			}
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{{ID: "other-user"}})
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "secret", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	err = admin.ensureBrokerIdentity(context.Background(), "admin-token", keycloakUser{ID: "user-1"}, "identity-1")
	if err == nil || !strings.Contains(err.Error(), "other-user") {
		t.Fatalf("ensureBrokerIdentity() conflict error = %v", err)
	}
}

func TestEnsureBrokerIdentityDoesNotDeleteStaleLinkWhenTargetHasAnotherOwner(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /admin/realms/acme/users/user-1/federated-identity":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{{IdentityProvider: platformBrokerAlias, UserID: "old-identity"}})
		case "GET /admin/realms/acme/users":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakUser{{ID: "other-user"}})
		case "DELETE /admin/realms/acme/users/user-1/federated-identity/basic-platform", "POST /admin/realms/acme/users/user-1/federated-identity/basic-platform":
			mutations++
			writer.WriteHeader(stdhttp.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	admin, err := NewKeycloakAdmin(server.URL, "acme", "admin", "secret", server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	err = admin.ensureBrokerIdentity(context.Background(), "admin-token", keycloakUser{ID: "user-1"}, "identity-1")
	if err == nil || !strings.Contains(err.Error(), "other-user") {
		t.Fatalf("ensureBrokerIdentity() stale ownership error = %v", err)
	}
	if mutations != 0 {
		t.Fatalf("stale link was mutated before ownership was proven safe: mutations=%d", mutations)
	}
}

func TestKeycloakAdminUsesServiceAccountClientCredentialsWhenConfigured(t *testing.T) {
	// Environment values must never influence an explicitly composed adapter.
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "ignored-environment-client")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "ignored-environment-secret")
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

	admin, err := NewKeycloakAdminWithCredentials(server.URL, "acme", KeycloakAdminCredentials{
		ServiceAccountClientID:     "platform-admin",
		ServiceAccountClientSecret: "client-secret",
	}, server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakAdmin: %v", err)
	}
	if _, err := admin.token(context.Background()); err != nil {
		t.Fatalf("token() error = %v", err)
	}
}

func TestNewKeycloakAdminWithCredentialsRejectsIncompleteServiceAccountPair(t *testing.T) {
	_, err := NewKeycloakAdminWithCredentials("http://keycloak.example", "acme", KeycloakAdminCredentials{
		ServiceAccountClientID: "platform-admin",
	})
	if err == nil || !strings.Contains(err.Error(), "complete pair") {
		t.Fatalf("NewKeycloakAdminWithCredentials error = %v", err)
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
		case "GET /admin/realms/acme/users/user-1/federated-identity":
			writeJSON(t, writer, stdhttp.StatusOK, []keycloakFederatedIdentity{{IdentityProvider: platformBrokerAlias, UserID: "identity-1", UserName: "platform-identity-1"}})
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
