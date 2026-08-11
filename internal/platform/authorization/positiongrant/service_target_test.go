package positiongrant

import (
	"reflect"
	"testing"
)

func TestAuthorizationTargetViewsExposePlatformNativeRolesAsReady(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID:   "platform-app",
			ApplicationCode: "platform",
			ApplicationName: "基础能力平台",
			RoleID:          "role-security-admin",
			RoleCode:        "platform-security-admin",
			RoleName:        "平台安全管理员",
			RoleType:        roleTypePlatform,
			RoleStatus:      activeStatus,
		},
	})

	want := []AuthorizationTargetView{
		{
			ApplicationID:    "platform-app",
			ApplicationCode:  "platform",
			ApplicationName:  "基础能力平台",
			CatalogVersion:   "built-in",
			CatalogSyncState: catalogStatusSynced,
			Roles: []AuthorizationTargetRoleView{
				{
					RoleID:     "role-security-admin",
					RoleCode:   "platform-security-admin",
					RoleName:   "平台安全管理员",
					RoleType:   roleTypePlatform,
					RoleStatus: activeStatus,
					Assignable: true,
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorizationTargetViews() = %#v, want %#v", got, want)
	}
}

func TestAuthorizationTargetViewsPreserveSynchronizedSubsystemCatalogMetadata(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID:    "contract-app",
			ApplicationCode:  "contract_management",
			ApplicationName:  "合同管理系统",
			CatalogVersion:   "2026.07",
			CatalogSyncState: catalogStatusSynced,
			RoleID:           "role-sales",
			RoleCode:         "sales",
			RoleName:         "销售人员",
			RoleType:         roleTypeApplication,
			RoleStatus:       activeStatus,
		},
	})

	if len(got) != 1 {
		t.Fatalf("target count = %d, want 1", len(got))
	}
	if got[0].CatalogVersion != "2026.07" || got[0].CatalogSyncState != catalogStatusSynced {
		t.Fatalf("catalog metadata = %q/%q", got[0].CatalogVersion, got[0].CatalogSyncState)
	}
	if len(got[0].Roles) != 1 || got[0].Roles[0].RoleType != roleTypeApplication || !got[0].Roles[0].Assignable {
		t.Fatalf("unexpected roles: %#v", got[0].Roles)
	}
}
