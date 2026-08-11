package applicationaccess

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestOrganizationClaimsFromRowsRequiresStableDirectSet(t *testing.T) {
	primary, organizations, err := organizationClaimsFromRows([]organizationClaimRow{
		{OrganizationID: "org-a"},
		{OrganizationID: "org-b", IsPrimary: true},
	})
	if err != nil || primary != "org-b" || len(organizations) != 2 || organizations[0] != "org-a" || organizations[1] != "org-b" {
		t.Fatalf("organizationClaimsFromRows() = %q, %#v, %v", primary, organizations, err)
	}
	for name, rows := range map[string][]organizationClaimRow{
		"duplicate":          {{OrganizationID: "org-a"}, {OrganizationID: "org-a"}},
		"unsorted":           {{OrganizationID: "org-b"}, {OrganizationID: "org-a"}},
		"multiple primaries": {{OrganizationID: "org-a", IsPrimary: true}, {OrganizationID: "org-b", IsPrimary: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := organizationClaimsFromRows(rows); err == nil {
				t.Fatal("invalid organization rows accepted")
			}
		})
	}
}

func TestOrganizationClaimsQueryUsesOnlyCurrentDirectMemberships(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{
		DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	statement := buildOrganizationClaimsQuery(database, "tenant-1", "user-1", now).Find(&[]organizationClaimRow{}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{
		"FROM iam_membership AS membership",
		"JOIN iam_user AS user",
		"JOIN iam_org_unit AS organization",
		"membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ?",
		"membership.valid_from IS NULL OR membership.valid_from <= ?",
		"membership.valid_until IS NULL OR membership.valid_until > ?",
		"GROUP BY `membership`.`org_unit_id`",
		"ORDER BY membership.org_unit_id ASC",
		"LIMIT ?",
	} {
		if !strings.Contains(sql, expected) {
			t.Errorf("organization claims SQL missing %q: %s", expected, sql)
		}
	}
	for _, forbidden := range []string{"organization.path", "parent_id", "WITH RECURSIVE"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("organization claims SQL must not expand descendants via %q: %s", forbidden, sql)
		}
	}
	if len(statement.Vars) == 0 || statement.Vars[0] != activeStatus {
		t.Fatalf("organization claims query does not bind ACTIVE state: %#v", statement.Vars)
	}
}
