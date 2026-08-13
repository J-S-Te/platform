package infrastructure

import (
	"strings"
	"testing"
)

func TestKeycloakClientConfigurationHashBindsRealmClientAndSchema(t *testing.T) {
	baseline := keycloakClientConfigurationHash("basic-platform", "orders-prod-web", "schema-v1")
	if len(baseline) != 64 {
		t.Fatalf("configuration hash length = %d, want 64", len(baseline))
	}
	for name, hash := range map[string]string{
		"realm":  keycloakClientConfigurationHash("other-realm", "orders-prod-web", "schema-v1"),
		"client": keycloakClientConfigurationHash("basic-platform", "orders-v2-prod-web", "schema-v1"),
		"schema": keycloakClientConfigurationHash("basic-platform", "orders-prod-web", "schema-v2"),
		"redirect": keycloakClientConfigurationHash(
			"basic-platform", "orders-prod-web", "schema-v1", "https://orders.example.com", "/orders", "catalog-a", "claims-a",
		),
		"catalog": keycloakClientConfigurationHash(
			"basic-platform", "orders-prod-web", "schema-v1", "", "", "catalog-b", "claims-a",
		),
	} {
		if hash == baseline {
			t.Fatalf("%s change did not change configuration hash", name)
		}
	}
	if got := keycloakClientConfigurationHash(" basic-platform ", " orders-prod-web ", " schema-v1 "); got != baseline {
		t.Fatalf("configuration hash did not normalize surrounding whitespace: %q", got)
	}
}

func TestManagedClientConfigurationQueryBindsEnvironmentAndCatalog(t *testing.T) {
	database := newDryRunMySQL(t)
	var configuration managedClientConfiguration
	result := database.Table("platform_application_environment AS environment").
		Select(`COALESCE(environment.base_url, '') AS base_url,
			COALESCE(environment.path_prefix, '') AS path_prefix,
			COALESCE(catalog.catalog_hash, '') AS catalog_hash,
			COALESCE(catalog.claims_role_config_hash, '') AS claims_role_config_hash`).
		Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id = environment.tenant_id AND catalog.application_id = environment.application_id").
		Where("environment.tenant_id = ? AND environment.application_id = ? AND environment.id = ?", "tenant", "application", "environment").
		Take(&configuration)
	sql := result.Statement.SQL.String()
	for _, fragment := range []string{"base_url", "path_prefix", "catalog_hash", "claims_role_config_hash", "environment.tenant_id = ?", "environment.application_id = ?", "environment.id = ?"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("managed configuration query missing %q: %s", fragment, sql)
		}
	}
}

func TestConfigurationReconcileIncludesRetainedDisabledUsers(t *testing.T) {
	query := allKeycloakConfigurationUsersQuery(newDryRunMySQL(t), "tenant").Find(&[]struct{}{})
	sql := query.Statement.SQL.String()
	if !strings.Contains(sql, "tenant_id = ?") || !strings.Contains(sql, "ORDER BY id ASC") {
		t.Fatalf("configuration reconcile query is not tenant-bound and stable: %s", sql)
	}
	if strings.Contains(sql, "status = ?") || strings.Contains(sql, "deleted_at IS NULL") {
		t.Fatalf("configuration reconcile excluded retained disabled identities: %s", sql)
	}
}

func TestStoredClientMappingReconcileOnlyLoadsSynchronizedMappingsInStableOrder(t *testing.T) {
	query := syncedKeycloakClientMappingsQuery(newDryRunMySQL(t)).Find(&[]storedClientMapping{})
	sql := query.Statement.SQL.String()
	if !strings.Contains(sql, "status = ?") {
		t.Fatalf("stored mapping reconcile does not filter synchronized mappings: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY tenant_id ASC, application_id ASC, environment_id ASC") {
		t.Fatalf("stored mapping reconcile order is unstable: %s", sql)
	}
}

func TestKeycloakClientConfigurationChangeDetection(t *testing.T) {
	currentHash := keycloakClientConfigurationHash("basic-platform", "orders-prod-web", "schema-v1")
	current := persistedClientMapping{Realm: "basic-platform", ClientID: "orders-prod-web", ConfigurationHash: currentHash}
	tests := []struct {
		name     string
		exists   bool
		previous persistedClientMapping
		realm    string
		clientID string
		hash     string
		changed  bool
	}{
		{name: "first mapping", realm: "basic-platform", clientID: "orders-prod-web", hash: currentHash, changed: true},
		{name: "unchanged mapping", exists: true, previous: current, realm: "basic-platform", clientID: "orders-prod-web", hash: currentHash},
		{name: "realm changed", exists: true, previous: current, realm: "other", clientID: "orders-prod-web", hash: currentHash, changed: true},
		{name: "client changed", exists: true, previous: current, realm: "basic-platform", clientID: "orders-v2-prod-web", hash: currentHash, changed: true},
		{name: "schema changed", exists: true, previous: current, realm: "basic-platform", clientID: "orders-prod-web", hash: "new-hash", changed: true},
		{name: "legacy hash missing", exists: true, previous: persistedClientMapping{Realm: "basic-platform", ClientID: "orders-prod-web"}, realm: "basic-platform", clientID: "orders-prod-web", hash: currentHash, changed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := keycloakClientConfigurationChanged(test.exists, test.previous, test.realm, test.clientID, test.hash); got != test.changed {
				t.Fatalf("changed = %t, want %t", got, test.changed)
			}
		})
	}
}

func TestSaveKeycloakClientMappingRejectsIncompleteConfiguration(t *testing.T) {
	store, err := NewClientMappingStore(newDryRunMySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveKeycloakClientMapping(t.Context(), "tenant", "application", "environment", "realm", ""); err == nil {
		t.Fatal("SaveKeycloakClientMapping() error = nil")
	}
}
