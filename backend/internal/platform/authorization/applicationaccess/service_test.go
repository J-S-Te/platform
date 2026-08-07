package applicationaccess

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSortedUniqueTrimsDeduplicatesAndSorts(t *testing.T) {
	got := sortedUnique([]string{" contract.read ", "", "contract.read", "contract.create"})
	want := []string{"contract.create", "contract.read"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExplicitPlatformRoleMappingIsReportedAsInherited(t *testing.T) {
	_, roles, direct, inherited := resolveAssignedRoles([]assignedRoleRow{{
		RoleID: "role-crm-admin", Code: "crm_super_admin", Name: "CRM 超级管理员",
		SubjectType: "PLATFORM_ROLE", SubjectID: "role-platform-admin", SourceName: "平台超级管理员",
		GrantOrigin: grantOriginInherited, OriginItemID: "mapping-1",
	}}, subjectTypeUser)
	if len(roles) != 1 || len(direct) != 0 || len(inherited) != 1 {
		t.Fatalf("roles/direct/inherited = %d/%d/%d", len(roles), len(direct), len(inherited))
	}
	if inherited[0].SourceKind != sourceKindInherited || inherited[0].Direct || inherited[0].OriginItemID != "mapping-1" {
		t.Fatalf("unexpected inherited platform-role view: %+v", inherited[0])
	}
}

func TestNormalizeRoleInputsAllowsMultipleDifferentRoles(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	got, err := normalizeRoleInputs([]RoleInput{
		{RoleCode: "audit_admin", ScopeType: "APPLICATION"},
		{RoleCode: "sales", ScopeType: "TENANT"},
	}, true, now)
	if err != nil {
		t.Fatalf("normalizeRoleInputs returned error: %v", err)
	}
	if len(got) != 2 || got[0].RoleCode != "audit_admin" || got[1].RoleCode != "sales" {
		t.Fatalf("unexpected normalized roles: %+v", got)
	}
}

func TestNormalizeRoleInputsRejectsRepeatedRoleAcrossScopes(t *testing.T) {
	_, err := normalizeRoleInputs([]RoleInput{
		{RoleCode: "sales", ScopeType: "APPLICATION"},
		{RoleCode: "sales", ScopeType: "ENVIRONMENT", EnvironmentCode: "prod"},
	}, true, time.Now().UTC())
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected repeated role validation error, got %v", err)
	}
}

func TestValidateMaximumRoleCount(t *testing.T) {
	tests := []struct {
		name    string
		maximum int
		roleIDs []string
		wantErr bool
	}{
		{name: "unlimited", maximum: 0, roleIDs: []string{"sales", "audit"}},
		{name: "same role from multiple sources", maximum: 1, roleIDs: []string{"sales", "sales"}},
		{name: "within maximum", maximum: 2, roleIDs: []string{"sales", "audit"}},
		{name: "exceeds maximum", maximum: 1, roleIDs: []string{"sales", "audit"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMaximumRoleCount(tt.maximum, tt.roleIDs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMaximumRoleCount(%d, %v) error = %v, wantErr %v", tt.maximum, tt.roleIDs, err, tt.wantErr)
			}
			if tt.wantErr && !errorsIs(err, ErrValidation) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}

func TestNormalizeRoleInputsAllowsEnvironmentScope(t *testing.T) {
	got, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ScopeType: "ENVIRONMENT", EnvironmentCode: "prod"}}, true, time.Now().UTC())
	if err != nil {
		t.Fatalf("normalizeRoleInputs returned error: %v", err)
	}
	if len(got) != 1 || got[0].ScopeType != "ENVIRONMENT" || got[0].EnvironmentCode != "prod" {
		t.Fatalf("unexpected normalized environment role: %+v", got)
	}
}

func TestNormalizeRoleInputsRejectsApplicationEnvironmentCode(t *testing.T) {
	_, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ScopeType: "APPLICATION", EnvironmentCode: "prod"}}, true, time.Now().UTC())
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNormalizeRoleInputsRejectsExpiredRole(t *testing.T) {
	expired := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	_, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ValidUntil: &expired}}, true, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestResolveAssignedRolesCombinesDirectOrganizationAndPositionSources(t *testing.T) {
	rows := []assignedRoleRow{
		{RoleID: "role-sales", Code: "sales", Name: "销售人员", ScopeType: scopeTypeTenant, SubjectType: subjectTypeUser, SubjectID: "user-1", SourceName: "张三"},
		{RoleID: "role-director", Code: "sales_director", Name: "销售总监", ScopeType: scopeTypeTenant, SubjectType: subjectTypeOrgUnit, SubjectID: "org-sales", SourceName: "销售部"},
		{RoleID: "role-audit", Code: "audit_admin", Name: "审计管理员", ScopeType: scopeTypeEnvironment, ScopeID: "env-prod", EnvironmentCode: "prod", SubjectType: subjectTypePosition, SubjectID: "position-audit", SourceName: "审计岗"},
	}

	roleIDs, roles, direct, inherited := resolveAssignedRoles(rows, subjectTypeUser)
	assertStrings(t, roleIDs, []string{"role-audit", "role-director", "role-sales"})
	if len(roles) != 3 || len(direct) != 1 || len(inherited) != 2 {
		t.Fatalf("roles=%+v direct=%+v inherited=%+v", roles, direct, inherited)
	}
	if !direct[0].Direct || direct[0].SourceType != subjectTypeUser || direct[0].SourceID != "user-1" || direct[0].SourceName != "张三" {
		t.Fatalf("unexpected direct source: %+v", direct[0])
	}
	if inherited[0].Direct || inherited[0].SourceType != subjectTypeOrgUnit || inherited[0].SourceName != "销售部" {
		t.Fatalf("unexpected organization source: %+v", inherited[0])
	}
	if inherited[1].Direct || inherited[1].SourceType != subjectTypePosition || inherited[1].SourceName != "审计岗" || inherited[1].EnvironmentCode != "prod" {
		t.Fatalf("unexpected position source: %+v", inherited[1])
	}
}

func TestResolveAssignedRolesPreservesAllSourcesForTheSameRole(t *testing.T) {
	rows := []assignedRoleRow{
		{RoleID: "role-sales", Code: "sales", ScopeType: scopeTypeTenant, SubjectType: subjectTypeUser, SubjectID: "user-1"},
		{RoleID: "role-sales", Code: "sales", ScopeType: scopeTypeTenant, SubjectType: subjectTypeOrgUnit, SubjectID: "org-sales"},
		{RoleID: "role-sales", Code: "sales", ScopeType: scopeTypeTenant, SubjectType: subjectTypePosition, SubjectID: "position-sales"},
	}

	roleIDs, roles, direct, inherited := resolveAssignedRoles(rows, subjectTypeUser)
	assertStrings(t, roleIDs, []string{"role-sales"})
	if len(roles) != 3 || len(direct) != 1 || len(inherited) != 2 {
		t.Fatalf("same role sources were lost: roles=%+v direct=%+v inherited=%+v", roles, direct, inherited)
	}
}

func TestResolveAssignedRolesPreservesDifferentOriginsOnSamePosition(t *testing.T) {
	rows := []assignedRoleRow{
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypePosition, SubjectID: "position-1", ScopeType: scopeTypeTenant, GrantOrigin: grantOriginManual},
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypePosition, SubjectID: "position-1", ScopeType: scopeTypeTenant, GrantOrigin: grantOriginTemplate, OriginID: "assignment-1", OriginItemID: "template-item-1"},
	}

	roleIDs, roles, direct, inherited := resolveAssignedRoles(rows, subjectTypeUser)
	assertStrings(t, roleIDs, []string{"role-sales"})
	if len(roles) != 2 || len(direct) != 0 || len(inherited) != 2 {
		t.Fatalf("manual and template provenance must both remain visible: roles=%+v direct=%+v inherited=%+v", roles, direct, inherited)
	}
	if roles[0].GrantOrigin != grantOriginManual || roles[1].GrantOrigin != grantOriginTemplate {
		t.Fatalf("unexpected preserved origins: %+v", roles)
	}
}

func TestApplicationAccessSubjectFilterRequiresEffectiveActiveMembership(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clause, args := applicationAccessSubjectFilter("user-1", now)
	for _, fragment := range []string{
		"rb.subject_type = ? AND rb.subject_id = ?",
		"rb.subject_type IN (?, ?)",
		"FROM iam_membership AS membership",
		"JOIN iam_org_unit AS organization",
		"organization.status = ?",
		"JOIN iam_position AS position",
		"position.org_unit_id = membership.org_unit_id",
		"position.status = ?",
		"membership.status = ?",
		"membership.valid_from IS NULL OR membership.valid_from <= ?",
		"membership.valid_until IS NULL OR membership.valid_until > ?",
		"membership.org_unit_id = rb.subject_id",
		"membership.position_id = rb.subject_id",
	} {
		if !strings.Contains(clause, fragment) {
			t.Errorf("subject filter is missing %q", fragment)
		}
	}
	want := []any{subjectTypeUser, "user-1", subjectTypeOrgUnit, subjectTypePosition, activeStatus, activeStatus, "user-1", activeStatus, true, now, now, subjectTypeOrgUnit, subjectTypePosition}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%#v, want %#v", args, want)
	}
}

func TestApplicationAccessScopeFilterSeparatesOIDCAndManagementReads(t *testing.T) {
	managementClause, managementArgs := applicationAccessScopeFilter("")
	if !strings.Contains(managementClause, "rb.scope_type = ?") || len(managementArgs) != 3 || managementArgs[2] != scopeTypeEnvironment {
		t.Fatalf("management scope filter=%q args=%#v", managementClause, managementArgs)
	}

	oidcClause, oidcArgs := applicationAccessScopeFilter("env-prod")
	want := []any{scopeTypeTenant, "", scopeTypeEnvironment, "env-prod"}
	if !strings.Contains(oidcClause, "rb.scope_id = ?") || !reflect.DeepEqual(oidcArgs, want) {
		t.Fatalf("OIDC scope filter=%q args=%#v, want %#v", oidcClause, oidcArgs, want)
	}
}

func TestDirectUserWriteFilterCannotMatchInheritedBindings(t *testing.T) {
	clause, args := directApplicationRoleBindingFilter("tenant-1", "application-1", "user-1")
	want := []any{"tenant-1", "application-1", subjectTypeUser, "user-1"}
	if !strings.Contains(clause, "rb.subject_type = ?") || !strings.Contains(clause, "rb.subject_id = ?") || !reflect.DeepEqual(args, want) {
		t.Fatalf("direct write filter=%q args=%#v, want %#v", clause, args, want)
	}
	for _, inheritedType := range []string{subjectTypeOrgUnit, subjectTypePosition} {
		for _, arg := range args {
			if arg == inheritedType {
				t.Fatalf("direct USER write filter unexpectedly contains inherited subject type %q", inheritedType)
			}
		}
	}
}

func TestManagedSubjectFilterTargetsOnlySelectedOrganizationOrPosition(t *testing.T) {
	for _, subjectType := range []string{subjectTypeOrgUnit, subjectTypePosition} {
		clause, args := subjectRoleBindingFilter("tenant-1", "application-1", subjectType, "subject-1")
		want := []any{"tenant-1", "application-1", subjectType, "subject-1"}
		if !strings.Contains(clause, "rb.subject_type = ?") || !reflect.DeepEqual(args, want) {
			t.Fatalf("subject filter=%q args=%#v, want %#v", clause, args, want)
		}
	}
}

func TestManualBindingFiltersProtectGrantOrigin(t *testing.T) {
	tests := []struct {
		name            string
		clause          string
		args            []any
		wantSubjectType string
		wantSubjectID   string
	}{
		{
			name: "user exception",
			clause: func() string {
				clause, _ := manualDirectApplicationRoleBindingFilter("tenant-1", "application-1", "user-1")
				return clause
			}(),
			args: func() []any {
				_, args := manualDirectApplicationRoleBindingFilter("tenant-1", "application-1", "user-1")
				return args
			}(),
			wantSubjectType: subjectTypeUser,
			wantSubjectID:   "user-1",
		},
		{
			name: "legacy organization cleanup",
			clause: func() string {
				clause, _ := manualSubjectRoleBindingFilter("tenant-1", "application-1", subjectTypeOrgUnit, "org-1")
				return clause
			}(),
			args: func() []any {
				_, args := manualSubjectRoleBindingFilter("tenant-1", "application-1", subjectTypeOrgUnit, "org-1")
				return args
			}(),
			wantSubjectType: subjectTypeOrgUnit,
			wantSubjectID:   "org-1",
		},
		{
			name: "legacy position cleanup",
			clause: func() string {
				clause, _ := manualSubjectRoleBindingFilter("tenant-1", "application-1", subjectTypePosition, "position-1")
				return clause
			}(),
			args: func() []any {
				_, args := manualSubjectRoleBindingFilter("tenant-1", "application-1", subjectTypePosition, "position-1")
				return args
			}(),
			wantSubjectType: subjectTypePosition,
			wantSubjectID:   "position-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.clause, "rb.grant_origin = ?") {
				t.Fatalf("manual mutation filter does not constrain grant origin: %q", tt.clause)
			}
			wantArgs := []any{"tenant-1", "application-1", tt.wantSubjectType, tt.wantSubjectID, grantOriginManual}
			if !reflect.DeepEqual(tt.args, wantArgs) {
				t.Fatalf("manual mutation args=%#v", tt.args)
			}
			for _, protectedOrigin := range []string{grantOriginTemplate, grantOriginSystem} {
				for _, arg := range tt.args {
					if arg == protectedOrigin {
						t.Fatalf("manual filter unexpectedly targets protected origin %q", protectedOrigin)
					}
				}
			}
		})
	}
}

func TestNormalizeManagedSubjectTypeOnlyAllowsOrganizationAndPosition(t *testing.T) {
	for _, input := range []string{"ORG_UNIT", " org_unit ", "POSITION", " position "} {
		got, err := normalizeManagedSubjectType(input)
		if err != nil || (got != subjectTypeOrgUnit && got != subjectTypePosition) {
			t.Fatalf("normalizeManagedSubjectType(%q)=%q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "USER", "ACCOUNT", "TENANT"} {
		if _, err := normalizeManagedSubjectType(input); err == nil || !errorsIs(err, ErrValidation) {
			t.Fatalf("normalizeManagedSubjectType(%q) expected validation error, got %v", input, err)
		}
	}
}

func TestSubjectAccessWritePolicyRejectsOrganizationAndPosition(t *testing.T) {
	for _, subjectType := range []string{subjectTypeOrgUnit, subjectTypePosition} {
		err := validateSubjectAccessWritePolicy(subjectType)
		if err == nil || !errorsIs(err, ErrValidation) {
			t.Fatalf("subject_type=%q expected validation error, got %v", subjectType, err)
		}
		if !strings.Contains(err.Error(), "position authorization template") {
			t.Fatalf("subject_type=%q returned unactionable error: %v", subjectType, err)
		}
	}
}

func TestUpdateSubjectAccessRejectsManualWriteBeforePersistence(t *testing.T) {
	// A service without persistence dependencies is intentional: the policy must reject the
	// historical write endpoint before any application, role, subject or binding query is made.
	service := &Service{}
	for _, subjectType := range []string{subjectTypeOrgUnit, subjectTypePosition} {
		_, err := service.UpdateSubjectAccess(context.Background(), UpdateSubjectAccessInput{
			TenantID:      "tenant-1",
			SubjectType:   subjectType,
			SubjectID:     "subject-1",
			OperatorID:    "operator-1",
			Roles:         []RoleInput{{RoleCode: "sales", ScopeType: "APPLICATION"}},
			RolesProvided: true,
		}, "contract_management")
		if err == nil || !errorsIs(err, ErrValidation) {
			t.Fatalf("subject_type=%q expected pre-persistence validation error, got %v", subjectType, err)
		}
	}
}

func TestSubjectAccessMarksSelectedSubjectRolesDirect(t *testing.T) {
	rows := []assignedRoleRow{{RoleID: "role-sales", Code: "sales", ScopeType: scopeTypeTenant, SubjectType: subjectTypeOrgUnit, SubjectID: "org-sales", SourceName: "销售部"}}
	_, roles, direct, inherited := resolveAssignedRoles(rows, subjectTypeOrgUnit)
	if len(roles) != 1 || len(direct) != 1 || len(inherited) != 0 || !direct[0].Direct {
		t.Fatalf("subject access must expose its own bindings as direct: roles=%+v direct=%+v inherited=%+v", roles, direct, inherited)
	}
}

func TestContractRolePolicyAllowsSameRoleFromMultipleSources(t *testing.T) {
	rows := []assignedRoleRow{
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypeUser, SubjectID: "user-1"},
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypeOrgUnit, SubjectID: "org-sales"},
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypePosition, SubjectID: "position-sales"},
	}
	state, conflicts, roleIDs := applyApplicationRolePolicy(ApplicationAuthorizationPolicy{MaxEffectiveRoles: 1}, rows, []string{"role-sales", "role-sales"})
	if state != authorizationGranted || len(conflicts) != 0 {
		t.Fatalf("state=%q conflicts=%v", state, conflicts)
	}
	assertStrings(t, roleIDs, []string{"role-sales"})
}

func TestContractRolePolicyFailsClosedForDifferentRoles(t *testing.T) {
	rows := []assignedRoleRow{
		{RoleID: "role-sales", Code: "sales", SubjectType: subjectTypeUser, SubjectID: "user-1"},
		{RoleID: "role-director", Code: "sales_director", SubjectType: subjectTypePosition, SubjectID: "position-director"},
	}
	state, conflicts, permissionRoleIDs := applyApplicationRolePolicy(ApplicationAuthorizationPolicy{MaxEffectiveRoles: 1}, rows, []string{"role-sales", "role-director"})
	if state != authorizationConflict {
		t.Fatalf("state=%q, want %q", state, authorizationConflict)
	}
	assertStrings(t, conflicts, []string{"sales", "sales_director"})
	if len(permissionRoleIDs) != 0 {
		t.Fatalf("conflicting contract roles must not be used for permission union: %v", permissionRoleIDs)
	}
}

func TestContractRolePolicyTreatsNoRoleAsUnauthorized(t *testing.T) {
	state, conflicts, roleIDs := applyApplicationRolePolicy(ApplicationAuthorizationPolicy{MaxEffectiveRoles: 1}, nil, nil)
	if state != authorizationUnauthorized || len(conflicts) != 0 || len(roleIDs) != 0 {
		t.Fatalf("state=%q conflicts=%v roleIDs=%v", state, conflicts, roleIDs)
	}
}

func TestOIDCAuthorizationGateFailsClosedForUnauthorizedAndConflict(t *testing.T) {
	for _, state := range []string{authorizationUnauthorized, authorizationConflict, ""} {
		if err := requireGrantedAuthorization(Access{AuthorizationState: state}); !errorsIs(err, ErrAccessDenied) {
			t.Fatalf("state=%q expected access denied, got %v", state, err)
		}
	}
	if err := requireGrantedAuthorization(Access{AuthorizationState: authorizationGranted}); err != nil {
		t.Fatalf("granted authorization unexpectedly rejected: %v", err)
	}
}

func TestOtherApplicationRolePolicyKeepsMultiRoleUnion(t *testing.T) {
	rows := []assignedRoleRow{{RoleID: "role-a", Code: "a"}, {RoleID: "role-b", Code: "b"}}
	state, conflicts, roleIDs := applyApplicationRolePolicy(ApplicationAuthorizationPolicy{}, rows, []string{"role-b", "role-a", "role-a"})
	if state != authorizationGranted || len(conflicts) != 0 {
		t.Fatalf("state=%q conflicts=%v", state, conflicts)
	}
	assertStrings(t, roleIDs, []string{"role-a", "role-b"})
}

func TestEmptyAccessUsesStableArrayAndAuthorizationContract(t *testing.T) {
	access := emptyAccess("application-x", "prod", "hash", 7)
	if access.AuthorizationState != authorizationUnauthorized || access.Roles == nil || access.DirectRoles == nil || access.InheritedRoles == nil || access.Conflicts == nil {
		t.Fatalf("unexpected empty access: %+v", access)
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSynchronizedRolePolicyAcceptsApplicationOwnedRole(t *testing.T) {
	rows := []assignedRoleRow{{RoleID: "role-application-owned-v2", Code: "role_v2"}}
	state, conflicts, roleIDs := applyApplicationRolePolicy(ApplicationAuthorizationPolicy{MaxEffectiveRoles: 1}, rows, []string{"role-application-owned-v2"})
	if state != authorizationGranted {
		t.Fatalf("synchronized contract role state = %q, want %q", state, authorizationGranted)
	}
	assertStrings(t, conflicts, []string{})
	assertStrings(t, roleIDs, []string{"role-application-owned-v2"})
}

func TestCustomPermissionsAreRejectedForEveryApplication(t *testing.T) {
	if err := validateCustomPermissionsUpdate(false); err != nil {
		t.Fatalf("role-only update unexpectedly failed: %v", err)
	}
	if err := validateCustomPermissionsUpdate(true); err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("direct custom permissions must be rejected, got %v", err)
	}
}

func TestEffectivePermissionsAreReadOnlyRoleDerived(t *testing.T) {
	if _, err := effectivePermissionsForApplication(authorizationUnauthorized, nil); !errorsIs(err, ErrNotConfigured) {
		t.Fatalf("unconfigured user must not receive permissions, got %v", err)
	}

	effective, err := effectivePermissionsForApplication(authorizationGranted, []string{"contract.read", "contract.read", "contract.create"})
	if err != nil {
		t.Fatalf("role permission calculation unexpectedly failed: %v", err)
	}
	assertStrings(t, effective, []string{"contract.create", "contract.read"})
}

func TestApplicationPublishedClaimsRoleConfigHashIsOpaqueToThePlatform(t *testing.T) {
	const published = "sm3:4bf7340872f586174d367416be7914fccbd4af0a5e9da5179025a1e5bc01ea21"
	if published == roleConfigHash([]catalogRow{{RoleCode: "sales", PermissionCode: "contract.read", Effect: "ALLOW"}}) {
		t.Fatal("published application hash must remain opaque metadata, not a platform-derived business rule")
	}
}

func TestRoleConfigHashReflectsSynchronizedRolePermissionMappings(t *testing.T) {
	base := []catalogRow{
		{RoleCode: "sales_v2", PermissionCode: "contract.read", Effect: "ALLOW"},
		{RoleCode: "sales_v2", PermissionCode: "contract.create", Effect: "ALLOW"},
	}
	baseHash := roleConfigHash(base)
	if baseHash == "" {
		t.Fatal("synchronized catalog hash must not be empty")
	}

	metadataChanged := []catalogRow{
		{RoleCode: "sales_v2", RoleName: "销售人员（同步）", RoleType: "APPLICATION", RoleBuiltIn: false, PermissionCode: "contract.create", Effect: "ALLOW"},
		{RoleCode: "sales_v2", RoleName: "销售人员（同步）", RoleType: "APPLICATION", RoleBuiltIn: false, PermissionCode: "contract.read", Effect: "ALLOW"},
	}
	if got := roleConfigHash(metadataChanged); got != baseHash {
		t.Fatalf("catalog presentation metadata must not change token config hash: got %q, want %q", got, baseHash)
	}

	changedMapping := append(append([]catalogRow(nil), base...), catalogRow{RoleCode: "sales_v2", PermissionCode: "contract.delete", Effect: "ALLOW"})
	if got := roleConfigHash(changedMapping); got == baseHash {
		t.Fatal("changed synchronized role-permission mapping must change token config hash")
	}
}

func TestApplicationAccessRejectsPlatformControlPlaneRole(t *testing.T) {
	err := ensureApplicationAccessRole(roleRow{
		ID:       "role-super-admin",
		Code:     BootstrapSuperAdminRoleCode,
		Name:     "平台超级管理员",
		RoleType: "PLATFORM",
	})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("application access platform role error = %v, want validation rejection", err)
	}
}

func TestApplicationAccessAllowsApplicationOwnedRole(t *testing.T) {
	err := ensureApplicationAccessRole(roleRow{
		ID:       "role-contract-admin",
		Code:     "contract-admin",
		Name:     "合同管理员",
		RoleType: " application ",
	})
	if err != nil {
		t.Fatalf("application-owned role unexpectedly rejected: %v", err)
	}
}

func TestAccessAuditChangesReportsSecurityRelevantBeforeAndAfter(t *testing.T) {
	before := Access{
		Roles:                []RoleView{{Code: "sales"}},
		ManualRoles:          []RoleView{{Code: "sales"}},
		EffectivePermissions: []string{"contract.read"},
		AuthorizationState:   "ACTIVE",
	}
	after := Access{
		Roles:                []RoleView{{Code: "audit_admin"}, {Code: "sales"}},
		ManualRoles:          []RoleView{{Code: "audit_admin"}, {Code: "sales"}},
		EffectivePermissions: []string{"audit.read", "contract.read"},
		AuthorizationState:   "ACTIVE",
	}

	changes := accessAuditChanges(before, after)
	if len(changes) != 3 {
		t.Fatalf("change count = %d, want 3: %#v", len(changes), changes)
	}
	wantFields := []string{"manual_role_codes", "effective_role_codes", "effective_permissions"}
	for index, field := range wantFields {
		if changes[index].Field != field {
			t.Fatalf("change %d field = %q, want %q", index, changes[index].Field, field)
		}
	}
}

func TestAccessAuditChangesIgnoresRoleOrdering(t *testing.T) {
	before := Access{Roles: []RoleView{{Code: "sales"}, {Code: "audit_admin"}}}
	after := Access{Roles: []RoleView{{Code: "audit_admin"}, {Code: "sales"}}}
	if changes := accessAuditChanges(before, after); len(changes) != 0 {
		t.Fatalf("role ordering created false audit change: %#v", changes)
	}
}
