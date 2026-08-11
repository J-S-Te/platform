package managementscope

import (
	"strings"
	"testing"
	"time"
)

func TestScopeAllowsOnlyGrantedOrganizationsAndResources(t *testing.T) {
	scope := Scope{OrgUnitIDs: []string{"org-1"}, ResourceIDs: []string{"position-1"}}
	if !scope.Allows("org-1", "") {
		t.Fatal("organization grant must authorize resources in that organization")
	}
	if !scope.Allows("", "position-1") {
		t.Fatal("resource grant must authorize that exact resource")
	}
	if scope.Allows("org-2", "position-2") {
		t.Fatal("ungranted organization and resource must be denied")
	}
	if (Scope{}).Allows("org-1", "position-1") {
		t.Fatal("empty scope must fail closed")
	}
	if !(Scope{Unrestricted: true}).Allows("any-org", "any-resource") {
		t.Fatal("tenant scope must be unrestricted within the tenant")
	}
}

func TestManagementSubjectFilterUsesOneWellFormedMembershipExistsClause(t *testing.T) {
	filter := managementSubjectFilter()
	if got := strings.Count(filter, "AND EXISTS ("); got != 1 {
		t.Fatalf("membership EXISTS clause count = %d, want 1; filter=%s", got, filter)
	}
	for _, fragment := range []string{
		"membership.tenant_id = binding.tenant_id",
		"membership.inherit_authorization = 1",
		"position.org_unit_id = membership.org_unit_id",
		"membership.valid_from IS NULL OR membership.valid_from <= ?",
		"membership.valid_until IS NULL OR membership.valid_until > ?",
	} {
		if !strings.Contains(filter, fragment) {
			t.Fatalf("subject filter is missing %q", fragment)
		}
	}

	arguments := managementSubjectArguments("user-1", "account-1", time.Now().UTC())
	if len(arguments) != 14 {
		t.Fatalf("subject filter argument count = %d, want 14", len(arguments))
	}
}

func TestAppendUniqueIgnoresDuplicatesAndEmptyValues(t *testing.T) {
	values := appendUnique(nil, "org-1")
	values = appendUnique(values, "org-1")
	values = appendUnique(values, "")
	if len(values) != 1 || values[0] != "org-1" {
		t.Fatalf("appendUnique result = %#v, want [org-1]", values)
	}
}
