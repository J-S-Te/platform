package infrastructure

import (
	"strings"
	"testing"
)

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
