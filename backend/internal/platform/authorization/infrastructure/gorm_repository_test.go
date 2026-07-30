package infrastructure

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/application"
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
		"membership.inherit_authorization = 1",
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

func TestProtectedRoleCode(t *testing.T) {
	t.Parallel()

	for _, code := range []string{
		"platform-super-admin",
		" PLATFORM-SUPER-ADMIN ",
		"platform-emergency-admin",
		"platform-break-glass-production",
	} {
		if !isProtectedRoleCode(code) {
			t.Errorf("isProtectedRoleCode(%q) = false, want true", code)
		}
	}

	for _, code := range []string{
		"platform-security-admin",
		"role-custom-admin",
		"platform-breakglass-admin",
		"",
	} {
		if isProtectedRoleCode(code) {
			t.Errorf("isProtectedRoleCode(%q) = true, want false", code)
		}
	}
}

func TestEnsureRoleEditableProtectsApplicationRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		role    roleModel
		wantErr error
	}{
		{
			name:    "contract catalog mirror role is not console editable",
			role:    roleModel{RoleType: "APPLICATION"},
			wantErr: application.ErrConflict,
		},
		{
			name:    "application role comparison tolerates storage formatting",
			role:    roleModel{RoleType: " application "},
			wantErr: application.ErrConflict,
		},
		{
			name:    "built in role remains protected",
			role:    roleModel{RoleType: "BUILT_IN", BuiltIn: true},
			wantErr: application.ErrConflict,
		},
		{
			name: "custom role remains editable",
			role: roleModel{RoleType: "CUSTOM", BuiltIn: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ensureRoleEditable(tt.role)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ensureRoleEditable(%+v) error = %v, want %v", tt.role, err, tt.wantErr)
			}
		})
	}
}

func TestEnsureApplicationRoleBindingManaged(t *testing.T) {
	t.Parallel()

	if err := ensureApplicationRoleBindingManaged(roleModel{RoleType: "APPLICATION"}); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("application catalog role binding error = %v, want conflict", err)
	}
	if err := ensureApplicationRoleBindingManaged(roleModel{RoleType: "CUSTOM"}); err != nil {
		t.Fatalf("custom platform role binding error = %v, want nil", err)
	}
}

func TestCatalogMirrorReadOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  string
		version string
		hash    string
		want    bool
	}{
		{name: "not synchronized", status: "NOT_SYNCED"},
		{name: "synchronized", status: "SYNCED", want: true},
		{name: "failed resync retains version", status: "FAILED", version: "2026.07.30", want: true},
		{name: "failed resync retains hash", status: "FAILED", hash: "sha256:abc", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catalogMirrorReadOnly(tt.status, tt.version, tt.hash); got != tt.want {
				t.Fatalf("catalogMirrorReadOnly(%q, %q, %q) = %v, want %v", tt.status, tt.version, tt.hash, got, tt.want)
			}
		})
	}
}
