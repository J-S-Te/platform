package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestDeploymentStateFromModelPreservesRetryContext(t *testing.T) {
	t.Parallel()
	initialAdminUserID := "01K10B00000000000000000001"
	assignedAt := time.Date(2026, time.August, 3, 8, 30, 0, 0, time.UTC)
	model := subsystemDeploymentStateModel{
		TenantID:                "01K10A00000000000000000001",
		ApplicationID:           "01K10C00000000000000000001",
		EnvironmentID:           "01K10D00000000000000000001",
		ApplicationCode:         "contract_management",
		Environment:             "prod",
		InitialAdminUserID:      &initialAdminUserID,
		InitialAccessAssignedAt: &assignedAt,
		Status:                  application.SubsystemDeploymentStatusReady,
	}

	state := deploymentStateFromModel(model)
	if state.ApplicationID != model.ApplicationID || state.EnvironmentID != model.EnvironmentID {
		t.Fatalf("deployment identifiers were not preserved: %#v", state)
	}
	if state.InitialAdminUserID != initialAdminUserID || state.InitialAccessAssignedAt == nil || !state.InitialAccessAssignedAt.Equal(assignedAt) {
		t.Fatalf("initial access context was not preserved: %#v", state)
	}
}

func TestPortalApplicationAccessFilterIncludesEffectiveMembershipSubjects(t *testing.T) {
	clause, args := portalApplicationAccessFilter("user-1")

	for _, fragment := range []string{
		"access_assignment.subject_type = 'USER'",
		"access_assignment.subject_type IN ('ORG_UNIT', 'POSITION')",
		"FROM iam_membership AS membership",
		"JOIN iam_org_unit AS organization",
		"organization.status = 'ACTIVE'",
		"JOIN iam_position AS position",
		"position.org_unit_id = membership.org_unit_id",
		"position.status = 'ACTIVE'",
		"membership.status = 'ACTIVE'",
		"membership.valid_from IS NULL OR membership.valid_from <= UTC_TIMESTAMP(3)",
		"membership.valid_until IS NULL OR membership.valid_until > UTC_TIMESTAMP(3)",
		"access_assignment.status = 'ACTIVE'",
		"access_assignment.valid_from IS NULL OR access_assignment.valid_from <= UTC_TIMESTAMP(3)",
		"access_assignment.valid_until IS NULL OR access_assignment.valid_until > UTC_TIMESTAMP(3)",
		"access_assignment.scope_type = 'TENANT'",
		"access_assignment.scope_type = 'ENVIRONMENT'",
		"assigned_role.status = 'ACTIVE'",
		"application.code <> 'contract_management'",
		"COUNT(DISTINCT contract_role.code)",
		") = 1",
	} {
		if !strings.Contains(clause, fragment) {
			t.Errorf("portal access filter is missing %q", fragment)
		}
	}

	if len(args) != 5 {
		t.Fatalf("argument count = %d, want 5", len(args))
	}
	for index, argument := range args {
		if argument != "user-1" {
			t.Fatalf("argument %d = %#v, want user-1", index, argument)
		}
	}
}
