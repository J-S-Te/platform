package applicationaccess

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestNormalizeCatalogCanonicalizesAndHashesDeterministically(t *testing.T) {
	first, checksum1, err := normalizeCatalog(CatalogInput{
		CatalogVersion: " 2026.07.29 ",
		Permissions: []PermissionInput{
			{Code: "customer.read", Name: "查看客户", Action: "read", ResourceCode: "customer"},
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"},
		},
		Roles: []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"customer.read", "contract.read", "customer.read"}}},
	})
	if err != nil {
		t.Fatalf("normalizeCatalog returned error: %v", err)
	}
	second, checksum2, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "2026.07.29",
		Permissions: []PermissionInput{
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract", RiskLevel: "LOW"},
			{Code: "customer.read", Name: "查看客户", Action: "read", ResourceCode: "customer", RiskLevel: "LOW"},
		},
		Roles: []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"contract.read", "customer.read"}}},
	})
	if err != nil {
		t.Fatalf("normalizeCatalog returned error: %v", err)
	}
	if checksum1 != checksum2 || len(first.Permissions) != 2 || len(second.Roles[0].Permissions) != 2 {
		t.Fatalf("catalog normalization is not deterministic: %#v %s %#v %s", first, checksum1, second, checksum2)
	}
	if checksum1 == "" {
		t.Fatalf("checksum should not be empty")
	}
}

func TestNormalizeCatalogClaimsRoleConfigHashIsCanonicalAndValidated(t *testing.T) {
	input, checksum, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "1", ClaimsRoleConfigHash: "  sm3:abc_123  ",
		Permissions: []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
	})
	if err != nil {
		t.Fatalf("normalizeCatalog returned error: %v", err)
	}
	if input.ClaimsRoleConfigHash != "sm3:abc_123" || checksum == "" {
		t.Fatalf("unexpected claims hash normalization: %#v checksum=%q", input, checksum)
	}
	_, _, err = normalizeCatalog(CatalogInput{CatalogVersion: "1", ClaimsRoleConfigHash: "bad hash", Permissions: input.Permissions})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("invalid claims role config hash must be rejected, got %v", err)
	}
}

func TestNormalizeCatalogRejectsUnknownRolePermission(t *testing.T) {
	_, _, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "1",
		Permissions:    []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
		Roles:          []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"contract.write"}}},
	})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNormalizeCatalogRejectsDuplicateResourceAction(t *testing.T) {
	_, _, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "1",
		Permissions: []PermissionInput{
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"},
			{Code: "contract.view", Name: "浏览合同", Action: "READ", ResourceCode: "contract"},
		},
	})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestCatalogPermissionRenameFallbackUsesResourceActionIdentity(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{
		DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	statement := catalogPermissionByResourceAction(database, "tenant-1", "app-1", "resource-1", "manage").Take(&struct{ ID string }{}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"tenant_id = ?", "application_id = ?", "resource_id = ?", "action = ?"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("rename fallback query missing %q: %s", expected, sql)
		}
	}
	if len(statement.Vars) < 4 || statement.Vars[0] != "tenant-1" || statement.Vars[1] != "app-1" || statement.Vars[2] != "resource-1" || statement.Vars[3] != "manage" {
		t.Fatalf("rename fallback query binds unexpected values: %#v", statement.Vars)
	}
}

func TestContractCatalogUsesTheGenericApplicationOwnedPayload(t *testing.T) {
	input, checksum, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "2026.07.29.1",
		Permissions: []PermissionInput{
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"},
		},
		Roles: []CatalogRoleInput{{Code: "sales", Name: "销售人员", Permissions: []string{"contract.read"}}},
	})
	if err != nil {
		t.Fatalf("contract catalog must use the generic synchronization payload: %v", err)
	}
	if checksum == "" || len(input.Roles) != 1 || input.Roles[0].Code != "sales" {
		t.Fatalf("unexpected normalized contract catalog: %#v, checksum=%q", input, checksum)
	}
}

func TestCatalogSyncApplicationPrincipalRequiresMatchingScopedApplicationBearer(t *testing.T) {
	applicationID := "app-contract"
	valid := appctx.Principal{
		OAuthClientID: "oauth-client-1", ClientID: "catalog-publisher", TenantID: "tenant-1",
		ApplicationID: applicationID, ApplicationCode: "contract_management",
		EnvironmentID: "env-prod", EnvironmentCode: "prod",
		Scopes: map[string]struct{}{"authorization.catalog.sync": {}},
	}

	t.Run("accepts contract machine bearer and derives server provenance", func(t *testing.T) {
		request := httptest.NewRequest("PUT", "/api/v1/applications/"+applicationID+"/authorization-catalog", nil)
		request = request.WithContext(appctx.WithPrincipal(request.Context(), valid))
		principal, err := catalogSyncApplicationPrincipal(request, applicationID)
		if err != nil {
			t.Fatalf("catalog sync principal was rejected: %v", err)
		}
		if principal.ClientID != valid.ClientID || principal.TenantID != valid.TenantID {
			t.Fatalf("unexpected principal: %#v", principal)
		}
		sourceType, sourceIdentifier := catalogSourceFromApplicationPrincipal(principal)
		if sourceType != "APPLICATION" || sourceIdentifier != "oauth_client:"+valid.OAuthClientID {
			t.Fatalf("unexpected trusted catalog provenance: %q, %q", sourceType, sourceIdentifier)
		}
	})

	t.Run("rejects console principal even when it has application update permission", func(t *testing.T) {
		request := httptest.NewRequest("PUT", "/api/v1/applications/"+applicationID+"/authorization-catalog", nil)
		request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
			Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "admin-1"},
			PermissionCodes: []string{"platform:application:update"},
		}))
		if _, err := catalogSyncApplicationPrincipal(request, applicationID); err == nil || !errorsIs(err, ErrAccessDenied) {
			t.Fatalf("console principal must not sync a catalog, got %v", err)
		}
	})

	t.Run("rejects different application and missing scope", func(t *testing.T) {
		wrongApplication := valid
		wrongApplication.ApplicationID = "another-app"
		request := httptest.NewRequest("PUT", "/api/v1/applications/"+applicationID+"/authorization-catalog", nil)
		request = request.WithContext(appctx.WithPrincipal(context.Background(), wrongApplication))
		if _, err := catalogSyncApplicationPrincipal(request, applicationID); err == nil || !errorsIs(err, ErrAccessDenied) {
			t.Fatalf("mismatched application bearer must be rejected, got %v", err)
		}

		missingScope := valid
		missingScope.Scopes = map[string]struct{}{"authorization.catalog.read": {}}
		request = httptest.NewRequest("PUT", "/api/v1/applications/"+applicationID+"/authorization-catalog", nil)
		request = request.WithContext(appctx.WithPrincipal(context.Background(), missingScope))
		if _, err := catalogSyncApplicationPrincipal(request, applicationID); err == nil || !errorsIs(err, ErrAccessDenied) {
			t.Fatalf("application bearer without sync scope must be rejected, got %v", err)
		}
	})
}

func TestNormalizeCatalogPolicyIsIncludedInChecksum(t *testing.T) {
	unlimited, unlimitedChecksum, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "2026.07.29.1",
		Permissions:    []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
		Roles:          []CatalogRoleInput{{Code: "sales", Name: "销售人员", Permissions: []string{"contract.read"}}},
	})
	if err != nil {
		t.Fatalf("normalize unlimited catalog: %v", err)
	}
	limited, limitedChecksum, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "2026.07.29.1",
		Permissions:    []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
		Roles:          []CatalogRoleInput{{Code: "sales", Name: "销售人员", Permissions: []string{"contract.read"}}},
		Policy:         CatalogPolicyInput{MaxEffectiveRoles: 1},
	})
	if err != nil {
		t.Fatalf("normalize limited catalog: %v", err)
	}
	if unlimited.Policy.MaxEffectiveRoles != 0 || limited.Policy.MaxEffectiveRoles != 1 {
		t.Fatalf("unexpected normalized policies: unlimited=%+v limited=%+v", unlimited.Policy, limited.Policy)
	}
	if unlimitedChecksum == limitedChecksum {
		t.Fatal("catalog checksum must include max_effective_roles")
	}
}

func TestNormalizeCatalogRejectsInvalidMaxEffectiveRoles(t *testing.T) {
	for _, limit := range []int{-1, maxAuthorizationPolicyEffectiveRoles + 1} {
		_, _, err := normalizeCatalog(CatalogInput{
			CatalogVersion: "1",
			Permissions:    []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
			Policy:         CatalogPolicyInput{MaxEffectiveRoles: limit},
		})
		if err == nil || !errorsIs(err, ErrValidation) {
			t.Fatalf("limit %d: expected validation error, got %v", limit, err)
		}
	}
}

func TestPlatformCatalogConstantsAreStableAndWellFormed(t *testing.T) {
	// The bootstrap mirror exposes these constants to the API bootstrap, the platform audit
	// adapter, and the on-disk catalog row. Drift between Go and the migrations would be
	// silent; lock the values down with a unit test so any change forces a conscious review.
	if PlatformApplicationCode != "platform" {
		t.Fatalf("PlatformApplicationCode drift: got %q want %q", PlatformApplicationCode, "platform")
	}
	if PlatformCatalogVersion != "v1-platform-builtin" {
		t.Fatalf("PlatformCatalogVersion drift: got %q want %q", PlatformCatalogVersion, "v1-platform-builtin")
	}
	if PlatformCatalogSourceType != "BUILTIN" {
		t.Fatalf("PlatformCatalogSourceType drift: got %q want %q", PlatformCatalogSourceType, "BUILTIN")
	}
	if PlatformCatalogSourceIdentifier != "platform:bootstrap" {
		t.Fatalf("PlatformCatalogSourceIdentifier drift: got %q want %q", PlatformCatalogSourceIdentifier, "platform:bootstrap")
	}
	if BootstrapSuperAdminRoleCode != "platform-super-admin" {
		t.Fatalf("BootstrapSuperAdminRoleCode drift: got %q want %q", BootstrapSuperAdminRoleCode, "platform-super-admin")
	}
	if got := len(PlatformCatalogBootstrapOperatorID); got != 26 {
		t.Fatalf("PlatformCatalogBootstrapOperatorID must be a 26-char ULID placeholder to fit CHAR(26) last_synced_by; got length=%d value=%q", got, PlatformCatalogBootstrapOperatorID)
	}
}
