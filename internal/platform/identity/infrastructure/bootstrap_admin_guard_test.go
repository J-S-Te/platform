package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

func TestMembershipUpdateViolatesBlocksProtectedAppointmentChanges(t *testing.T) {
	identity := bootstrapadmin.Identity{TenantID: "tenant-1", UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	existing := membershipModel{ID: "membership-1", UserID: "user-bootstrap", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary, Status: domain.StatusActive}
	disabled := domain.StatusDisabled
	expiry := time.Now().UTC().Add(24 * time.Hour)

	cases := []struct {
		name     string
		existing membershipModel
		input    application.MembershipUpdateInput
		want     bool
	}{
		{
			name:     "disable protected appointment",
			existing: existing,
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-bootstrap", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary,
			}, MembershipID: "membership-1", Status: &disabled},
			want: true,
		},
		{
			name:     "expire protected appointment",
			existing: existing,
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-bootstrap", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary, EffectiveTo: &expiry,
			}, MembershipID: "membership-1"},
			want: true,
		},
		{
			name:     "transfer protected appointment to another user",
			existing: existing,
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-ordinary", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary,
			}, MembershipID: "membership-1"},
			want: true,
		},
		{
			name:     "move protected appointment to another organization",
			existing: existing,
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-bootstrap", OrgUnitID: "org-other", PositionID: "position-root", MembershipType: domain.MembershipPrimary,
			}, MembershipID: "membership-1"},
			want: true,
		},
		{
			name:     "inherit switch stays allowed",
			existing: existing,
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-bootstrap", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary,
			}, MembershipID: "membership-1"},
			want: false,
		},
		{
			name:     "ordinary appointment keeps existing management capability",
			existing: membershipModel{ID: "membership-2", UserID: "user-ordinary", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary, Status: domain.StatusActive},
			input: application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{
				UserID: "user-ordinary", OrgUnitID: "org-root", PositionID: "position-root", MembershipType: domain.MembershipPrimary,
			}, MembershipID: "membership-2", Status: &disabled},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := membershipUpdateViolates(identity, tc.existing, tc.input); got != tc.want {
				t.Fatalf("membershipUpdateViolates() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBootstrapExclusionFiltersApplyIdentityPredicates(t *testing.T) {
	identity := bootstrapadmin.Identity{UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	database := newDryRunManagementDatabase(t)

	userResult := excludeBootstrapUser(database.Model(&userModel{}), identity, true).Find(&[]userModel{})
	if userResult.Error != nil {
		t.Fatalf("build user exclusion: %v", userResult.Error)
	}
	assertStatementContains(t, userResult.Statement.SQL.String(), "iam_user.id <> ?")
	assertValuesContain(t, userResult.Statement.Vars, "user-bootstrap")

	accountResult := excludeBootstrapAccount(database.Model(&accountModel{}), identity, true).Find(&[]accountModel{})
	if accountResult.Error != nil {
		t.Fatalf("build account exclusion: %v", accountResult.Error)
	}
	accountSQL := accountResult.Statement.SQL.String()
	assertStatementContains(t, accountSQL, "iam_account.id <> ?")
	assertStatementContains(t, accountSQL, "iam_account.user_id IS NULL OR iam_account.user_id <> ?")
	assertValuesContain(t, accountResult.Statement.Vars, "account-bootstrap")
	assertValuesContain(t, accountResult.Statement.Vars, "user-bootstrap")

	membershipResult := excludeBootstrapMembership(database.Table("iam_membership AS m"), identity, true).Find(&[]membershipModel{})
	if membershipResult.Error != nil {
		t.Fatalf("build membership exclusion: %v", membershipResult.Error)
	}
	assertStatementContains(t, membershipResult.Statement.SQL.String(), "m.user_id <> ?")
	assertValuesContain(t, membershipResult.Statement.Vars, "user-bootstrap")
}

func TestBootstrapExclusionFiltersDoNothingBeforeInitialization(t *testing.T) {
	identity := bootstrapadmin.Identity{UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	database := newDryRunManagementDatabase(t)

	result := excludeBootstrapUser(database.Model(&userModel{}), identity, false).Find(&[]userModel{})
	if result.Error != nil {
		t.Fatalf("build user exclusion: %v", result.Error)
	}
	if statement := result.Statement.SQL.String(); strings.Contains(statement, "iam_user.id <> ?") {
		t.Fatalf("uninitialized tenant must not filter users; SQL=%s", statement)
	}
}

func TestProtectedCascadeQueriesTargetTheInitializedIdentity(t *testing.T) {
	database := newDryRunManagementDatabase(t)

	subtree := protectedMembershipInOrganizationSubtreeQuery(database, "tenant-1", "user-bootstrap", "/org-root/").Find(&[]membershipModel{})
	if subtree.Error != nil {
		t.Fatalf("build subtree query: %v", subtree.Error)
	}
	subtreeSQL := subtree.Statement.SQL.String()
	for _, fragment := range []string{"iam_membership AS membership", "iam_org_unit AS organization", "membership.user_id = ?", "membership.status = ?", "organization.path LIKE ?"} {
		assertStatementContains(t, subtreeSQL, fragment)
	}
	assertValuesContain(t, subtree.Statement.Vars, "user-bootstrap")
	assertValuesContain(t, subtree.Statement.Vars, "/org-root/%")

	primary := protectedPrimaryOrganizationInSubtreeQuery(database, "tenant-1", "user-bootstrap", "/org-root/").Find(&[]userModel{})
	if primary.Error != nil {
		t.Fatalf("build primary organization query: %v", primary.Error)
	}
	primarySQL := primary.Statement.SQL.String()
	for _, fragment := range []string{"iam_user AS user", "user.primary_org_id", "user.deleted_at IS NULL", "organization.path LIKE ?"} {
		assertStatementContains(t, primarySQL, fragment)
	}

	position := positionMembershipQuery(database, "tenant-1", "user-bootstrap", "position-root").Find(&[]membershipModel{})
	if position.Error != nil {
		t.Fatalf("build position query: %v", position.Error)
	}
	positionSQL := position.Statement.SQL.String()
	for _, fragment := range []string{"iam_membership", "user_id = ?", "position_id = ?", "status = ?"} {
		assertStatementContains(t, positionSQL, fragment)
	}
	assertValuesContain(t, position.Statement.Vars, "position-root")
}

func TestUserFilterConvertsAsciiEmployeeNumberForUnicodeKeyword(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	result := applyUserFilter(database.Model(&userModel{}), "tenant-1", application.PageRequest{Keyword: "张三"}).Find(&[]userModel{})
	if result.Error != nil {
		t.Fatalf("build user search: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{"display_name LIKE ?", "CONVERT(employee_no USING utf8mb4) LIKE ?", "email LIKE ?"} {
		assertStatementContains(t, statement, fragment)
	}
	assertValuesContain(t, result.Statement.Vars, "%张三%")
}

func assertStatementContains(t *testing.T, statement, fragment string) {
	t.Helper()
	if !strings.Contains(statement, fragment) {
		t.Fatalf("statement is missing %q; SQL=%s", fragment, statement)
	}
}

func assertValuesContain(t *testing.T, values []any, expected any) {
	t.Helper()
	for _, value := range values {
		if value == expected {
			return
		}
	}
	t.Fatalf("statement variables %#v are missing %#v", values, expected)
}
