package delegation

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func principalContext(tenantID, userID string, roleCodes ...string) context.Context {
	roles := make([]authctx.ReferenceName, 0, len(roleCodes))
	for _, code := range roleCodes {
		roles = append(roles, authctx.ReferenceName{Code: code})
	}
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: tenantID},
		User:   authctx.ReferenceName{ID: userID},
		Roles:  roles,
	})
}

func TestDelegationBypassPoliciesStayScopedByRoleAndTenant(t *testing.T) {
	cases := []struct {
		name            string
		ctx             context.Context
		tenantID        string
		operatorID      string
		standardAllowed bool
		templateAllowed bool
	}{
		{
			name:     "super administrator keeps the standard and template exception",
			ctx:      principalContext("tenant-1", "operator-1", "platform-super-admin"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: true, templateAllowed: true,
		},
		{
			name:     "security administrator is limited to position templates",
			ctx:      principalContext("tenant-1", "operator-1", "platform-security-admin"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: true,
		},
		{
			name:     "ordinary administrator receives no exception",
			ctx:      principalContext("tenant-1", "operator-1", "platform-audit-admin"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: false,
		},
		{
			name:     "role code must match exactly",
			ctx:      principalContext("tenant-1", "operator-1", "platform-security-admin-extra"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: false,
		},
		{
			name:     "tenant mismatch is rejected",
			ctx:      principalContext("tenant-2", "operator-1", "platform-security-admin"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: false,
		},
		{
			name:     "operator mismatch is rejected",
			ctx:      principalContext("tenant-1", "operator-2", "platform-security-admin"),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: false,
		},
		{
			name:     "missing authenticated principal is rejected",
			ctx:      context.Background(),
			tenantID: "tenant-1", operatorID: "operator-1",
			standardAllowed: false, templateAllowed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := standardDelegationBypass.allows(tc.ctx, tc.tenantID, tc.operatorID); got != tc.standardAllowed {
				t.Fatalf("standardDelegationBypass.allows() = %v, want %v", got, tc.standardAllowed)
			}
			if got := positionTemplateDelegationBypass.allows(tc.ctx, tc.tenantID, tc.operatorID); got != tc.templateAllowed {
				t.Fatalf("positionTemplateDelegationBypass.allows() = %v, want %v", got, tc.templateAllowed)
			}
		})
	}
}

func TestPlatformRolePredicatesUseAuthenticatedPrincipal(t *testing.T) {
	ctx := principalContext("tenant-1", "operator-1", "platform-super-admin", "platform-security-admin")
	if !IsPlatformSuperAdmin(ctx, "tenant-1", "operator-1") {
		t.Fatal("IsPlatformSuperAdmin() = false for a bound super administrator")
	}
	if !IsPlatformSecurityAdmin(ctx, "tenant-1", "operator-1") {
		t.Fatal("IsPlatformSecurityAdmin() = false for a bound security administrator")
	}
	if IsPlatformSecurityAdmin(ctx, "tenant-2", "operator-1") {
		t.Fatal("IsPlatformSecurityAdmin() = true for a different tenant")
	}
}
