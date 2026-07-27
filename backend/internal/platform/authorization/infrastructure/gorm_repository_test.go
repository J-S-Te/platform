package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/domain"
)

func TestRoleBindingSubjectFilterIncludesEffectiveOrganizationMembership(t *testing.T) {
	now := time.Date(2026, time.July, 26, 8, 0, 0, 0, time.UTC)
	clause, args := roleBindingSubjectFilter("user-1", "account-1", now)

	for _, fragment := range []string{
		"binding.subject_type IN (?, ?)",
		"FROM iam_membership AS membership",
		"JOIN iam_org_unit AS organization",
		"JOIN iam_position AS position",
		"membership.valid_from IS NULL OR membership.valid_from <= ?",
		"membership.valid_until IS NULL OR membership.valid_until > ?",
		"membership.org_unit_id = binding.subject_id",
		"membership.position_id = binding.subject_id",
	} {
		if !strings.Contains(clause, fragment) {
			t.Errorf("subject filter is missing %q", fragment)
		}
	}
	if len(args) != 14 {
		t.Fatalf("argument count = %d, want 14", len(args))
	}
	if args[0] != "USER" || args[1] != "user-1" || args[2] != "ACCOUNT" || args[3] != "account-1" {
		t.Fatalf("direct subject arguments = %#v", args[:4])
	}
	if args[6] != domain.StatusActive || args[7] != domain.StatusActive || args[9] != domain.StatusActive {
		t.Fatalf("active-state arguments = %#v", args[6:10])
	}
	if args[10] != now || args[11] != now {
		t.Fatalf("effective time arguments = %#v, want %v", args[10:12], now)
	}
}
