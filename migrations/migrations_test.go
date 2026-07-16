package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
)

func TestEmbeddedMigrationsCoverP0Tables(t *testing.T) {
	items, err := migration.Load(Files)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(items) != 11 {
		t.Fatalf("loaded %d migrations, want 11", len(items))
	}

	combined := make([]string, 0, len(items))
	for _, item := range items {
		statements, err := migration.SplitStatements(item.SQL)
		if err != nil {
			t.Fatalf("split migration %06d_%s: %v", item.Version, item.Name, err)
		}
		if len(statements) == 0 {
			t.Fatalf("migration %06d_%s has no executable SQL", item.Version, item.Name)
		}
		combined = append(combined, strings.ToLower(item.SQL))
	}

	schema := strings.Join(combined, "\n")
	for _, table := range []string{
		"iam_tenant",
		"platform_application",
		"platform_application_environment",
		"platform_oauth_client",
		"platform_oauth_redirect_uri",
		"platform_oauth_grant_type",
		"platform_oauth_client_scope",
		"platform_oauth_client_credential",
		"iam_user",
		"iam_account",
		"iam_password_credential",
		"iam_org_unit",
		"iam_position",
		"iam_membership",
		"iam_session",
		"authz_resource",
		"authz_permission",
		"authz_role",
		"authz_role_permission",
		"authz_role_binding",
		"authz_policy_revision",
		"audit_event_dedup",
		"audit_event",
		"cfg_namespace",
		"cfg_item",
		"cfg_release",
		"cfg_release_item",
		"file_object",
		"file_version",
		"file_binding",
		"async_job",
	} {
		if !strings.Contains(schema, "create table if not exists "+table) {
			t.Errorf("missing P0 table migration for %s", table)
		}
	}
}

func TestDefaultSeedAvoidsAStaticAdministratorPassword(t *testing.T) {
	content, err := Files.ReadFile("000011_seed_platform_defaults.sql")
	if err != nil {
		t.Fatalf("read default seed migration: %v", err)
	}
	seed := strings.ToLower(string(content))
	if strings.Contains(seed, "iam_password_credential") || strings.Contains(seed, "password_hash") {
		t.Fatal("default seed must not create a password credential")
	}
	for _, required := range []string{
		"insert into iam_tenant",
		"insert into platform_application",
		"insert into authz_resource",
		"insert into authz_permission",
		"insert into authz_role",
		"insert into cfg_namespace",
	} {
		if !strings.Contains(seed, required) {
			t.Errorf("default seed is missing %q", required)
		}
	}
}

func TestDefaultSeedContainsAllP0OpenAPIPermissions(t *testing.T) {
	content, err := Files.ReadFile("000011_seed_platform_defaults.sql")
	if err != nil {
		t.Fatalf("read default seed migration: %v", err)
	}
	seed := string(content)

	for _, permission := range []string{
		"platform:user:read",
		"platform:user:create",
		"platform:user:update",
		"platform:account:read",
		"platform:account:update",
		"platform:organization:read",
		"platform:organization:create",
		"platform:position:read",
		"platform:position:create",
		"platform:membership:read",
		"platform:membership:create",
		"platform:membership:update",
		"platform:resource:read",
		"platform:resource:create",
		"platform:permission:read",
		"platform:permission:create",
		"platform:role:read",
		"platform:role:create",
		"platform:role:update",
		"platform:role-binding:read",
		"platform:role-binding:create",
		"platform:role-binding:update",
		"platform:authorization:check",
		"platform:audit:view",
		"platform:audit:ingest",
		"platform:audit:export",
		"platform:config-namespace:read",
		"platform:config-namespace:create",
		"platform:config-item:read",
		"platform:config-item:create",
		"platform:config-item:update",
		"platform:config-release:publish",
		"platform:config-release:read",
		"platform:config:read",
	} {
		if !strings.Contains(seed, "'"+permission+"'") {
			t.Errorf("default seed is missing P0 permission %q", permission)
		}
	}
}

func TestDefaultSeedIdentifiersAreULIDs(t *testing.T) {
	content, err := Files.ReadFile("000011_seed_platform_defaults.sql")
	if err != nil {
		t.Fatalf("read default seed migration: %v", err)
	}

	seedIdentifierPattern := regexp.MustCompile(`'01[^']*'`)
	validULIDPattern := regexp.MustCompile(`^01[0-9A-HJKMNP-TV-Z]{24}$`)
	identifiers := seedIdentifierPattern.FindAllString(string(content), -1)
	if len(identifiers) == 0 {
		t.Fatal("default seed does not contain static identifiers")
	}

	for _, quotedIdentifier := range identifiers {
		identifier := strings.Trim(quotedIdentifier, "'")
		if !validULIDPattern.MatchString(identifier) {
			t.Errorf("seed identifier %q is not a 26-character ULID", identifier)
		}
	}
}
