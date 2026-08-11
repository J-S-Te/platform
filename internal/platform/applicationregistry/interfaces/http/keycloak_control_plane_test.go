package http

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestKeycloakIdentityClaimMappingsCoverCanonicalIdentityOrganizationAndAuthorization(t *testing.T) {
	got := make([]string, 0, len(keycloakIdentityClaimMappings))
	multivalued := make(map[string]bool, len(keycloakIdentityClaimMappings))
	for _, mapping := range keycloakIdentityClaimMappings {
		got = append(got, mapping.Name)
		multivalued[mapping.Name] = mapping.MultiValued
	}
	want := []string{
		"identity_id", "tenant_id", "person_id", "primary_org_id", "organization_ids",
		"roles", "permissions", "role_config_hash", "authz_revision",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapped claims = %#v, want %#v", got, want)
	}
	for _, claim := range []string{"organization_ids", "roles", "permissions"} {
		if !multivalued[claim] {
			t.Fatalf("%s must be configured as a multivalued claim", claim)
		}
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
