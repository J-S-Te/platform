package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	authzapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 脚本化数据库模拟同一已授权组织内一个在职攻击者和
// 一个有效高权限岗位，不连接数据库或 Keycloak。
type auditMembershipScenario struct {
	inserted      bool
	committed     bool
	queries       []string
	roleCode      string
	operatorRole  string
	delegated     bool
	noRoles       bool
	queryError    bool
	sourceInherit bool
	authority     string
}
type auditMembershipDriver struct{ s *auditMembershipScenario }
type auditMembershipConn struct{ s *auditMembershipScenario }
type auditMembershipTx struct{ s *auditMembershipScenario }

func (d auditMembershipDriver) Open(string) (driver.Conn, error) {
	return &auditMembershipConn{d.s}, nil
}
func (c *auditMembershipConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("no prepare")
}
func (c *auditMembershipConn) Close() error              { return nil }
func (c *auditMembershipConn) Begin() (driver.Tx, error) { return auditMembershipTx{c.s}, nil }
func (c auditMembershipTx) Commit() error                { c.s.committed = true; return nil }
func (c auditMembershipTx) Rollback() error              { return nil }
func (c *auditMembershipConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.s.queries = append(c.s.queries, q)
	switch {
	case strings.HasPrefix(q, "INSERT INTO `iam_membership`"):
		if !strings.Contains(q, "`inherit_authorization`") {
			return nil, fmt.Errorf("missing inheritance column")
		}
		c.s.inserted = true
		return membershipVersionResult(1), nil
	case strings.HasPrefix(q, "UPDATE `authz_policy_revision`"):
		return membershipVersionResult(1), nil
	default:
		return nil, fmt.Errorf("unexpected write: %s", q)
	}
}
func (c *auditMembershipConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.queries = append(c.s.queries, q)
	rows := func(cols []string, vals ...[]driver.Value) driver.Rows {
		return &membershipVersionRows{columns: cols, values: vals}
	}
	switch {
	case strings.Contains(q, "FROM `iam_bootstrap_state`"):
		return rows([]string{"tenant_id", "first_super_admin_user_id", "first_super_admin_account_id"}), nil
	case strings.Contains(q, "FROM `iam_tenant`"):
		return rows([]string{"code"}, []driver.Value{"synthetic-non-bootstrap"}), nil
	case strings.Contains(q, "FROM `iam_membership`"):
		return rows([]string{"id", "tenant_id", "user_id", "org_unit_id", "position_id", "membership_type", "status", "inherit_authorization", "version"}, []driver.Value{"existing-membership", "tenant-1", "attacker", "org-1", "old-position", "SECONDARY", "ACTIVE", c.s.sourceInherit, int64(1)}), nil
	case strings.Contains(q, "FROM `iam_personnel_change_request`"):
		return rows([]string{"id", "tenant_id", "user_id", "source_membership_id", "target_org_unit_id", "target_position_id", "change_type", "status", "effective_at", "submitted_by", "approved_by", "version"}, []driver.Value{"request-1", "tenant-1", "attacker", "existing-membership", "org-1", "privileged-position", "TRANSFER", "SCHEDULED", time.Now().UTC(), "applicant", "approver", int64(1)}), nil
	case strings.Contains(q, "FROM `iam_position`"):
		return rows([]string{"id", "org_unit_id", "tenant_id", "status"}, []driver.Value{"privileged-position", "org-1", "tenant-1", "ACTIVE"}), nil
	case strings.Contains(q, "FROM `iam_org_unit`"):
		return rows([]string{"id", "tenant_id", "status"}, []driver.Value{"org-1", "tenant-1", "ACTIVE"}), nil
	case strings.Contains(q, "FROM `platform_application`"):
		return rows([]string{"id", "tenant_id", "code", "status"}, []driver.Value{"platform-app", "tenant-1", "platform", "ACTIVE"}), nil
	case strings.Contains(q, "FROM `authz_role`"):
		return rows([]string{"id", "tenant_id", "application_id", "code", "status", "role_type"}, []driver.Value{"ordinary-role", "tenant-1", "platform-app", "ordinary", "ACTIVE", "APPLICATION"}), nil
	case strings.Contains(q, "FROM `authz_application_authorization_policy`"):
		return rows([]string{"max_effective_roles"}, []driver.Value{int64(10)}), nil
	case strings.Contains(q, "count(*)") && strings.Contains(q, "FROM `iam_account`"):
		return rows([]string{"count(*)"}, []driver.Value{int64(0)}), nil

	case strings.Contains(q, "FROM authz_role_binding AS binding") && strings.Contains(q, "role.application_id, role.code"):
		if c.s.queryError {
			return nil, fmt.Errorf("inherited catalog unavailable")
		}
		if c.s.noRoles {
			return rows([]string{"id", "application_id", "code"}), nil
		}
		code := c.s.roleCode
		if code == "" {
			code = "platform-super-admin"
		}
		return rows([]string{"id", "application_id", "code"}, []driver.Value{"target-role", "platform-app", code}), nil
	case strings.Contains(q, "count(*)") && strings.Contains(q, "JOIN iam_org_unit"):
		return rows([]string{"count(*)"}, []driver.Value{int64(1)}), nil
	case strings.Contains(q, "FROM `iam_user`") && strings.Contains(q, "FOR UPDATE"):
		return rows([]string{"id", "tenant_id", "status"}, []driver.Value{"attacker", "tenant-1", "ACTIVE"}), nil
	case strings.Contains(q, "FROM `keycloak_application_client_mapping`"):
		return rows([]string{"application_id", "environment_id"}), nil
	case strings.Contains(q, "FROM iam_membership AS m JOIN"):
		if !c.s.inserted {
			return nil, fmt.Errorf("membership queried before insertion")
		}
		return rows([]string{"id", "tenant_id", "user_id", "org_unit_id", "position_id", "membership_type", "status", "inherit_authorization", "version"}, []driver.Value{"new-membership", "tenant-1", "attacker", "org-1", "privileged-position", "SECONDARY", "ACTIVE", true, int64(1)}), nil
	case strings.Contains(q, "FROM authz_role_binding AS binding") && strings.Contains(q, "role.id, role.name, role.code"):
		for _, arg := range args {
			if arg.Value == "approver" {
				c.s.authority = "approver"
			}
		}
		if c.s.operatorRole != "" {
			return rows([]string{"id", "name", "code"}, []driver.Value{"operator-role", "operator", c.s.operatorRole}), nil
		}
		if !c.s.inserted {
			return rows([]string{"id", "name", "code"}), nil
		}
		if !strings.Contains(q, "membership.inherit_authorization = 1") {
			return nil, fmt.Errorf("unexpected principal filter")
		}
		return rows([]string{"id", "name", "code"}, []driver.Value{"super-role", "super", "platform-super-admin"}), nil
	case strings.Contains(q, "FROM authz_role_permission AS role_permission"):
		return rows([]string{"permission_id"}, []driver.Value{"target-permission"}), nil
	case strings.Contains(q, "COUNT(DISTINCT"):
		total := int64(0)
		if c.s.delegated {
			total = 1
		}
		return rows([]string{"count"}, []driver.Value{total}), nil
	default:
		return nil, fmt.Errorf("unexpected read: %s", q)
	}
}
func TestCreateMembershipRejectsInheritedProtectedRoleBeforeWriting(t *testing.T) {
	s := &auditMembershipScenario{}
	db := auditDelegationDatabase(t, s)
	repo, err := NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "attacker"}})
	before, err := repo.findRoles(ctx, "tenant-1", "attacker", "attacker-account", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("attacker started privileged")
	}
	got, err := repo.CreateMembership(ctx, application.MembershipCreateInput{TenantID: "tenant-1", OperatorID: "attacker", UserID: "attacker", OrgUnitID: "org-1", PositionID: "privileged-position", MembershipType: domain.MembershipSecondary}, "new-membership")
	if !errors.Is(err, authzapplication.ErrForbidden) {
		t.Fatalf("CreateMembership with inherited super role = %+v, %v; want ErrForbidden", got, err)
	}
	if s.inserted || s.committed {
		t.Fatal("forbidden membership must not be written or committed")
	}
}

var auditDelegationCounter uint64

func auditDelegationDatabase(t *testing.T, scenario *auditMembershipScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("membership-delegation-%d", atomic.AddUint64(&auditDelegationCounter, 1))
	sql.Register(name, auditMembershipDriver{scenario})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func TestMembershipDelegationChecksNewPermissionsAndKeepsSafeReductions(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	later := now.Add(2 * time.Hour)
	base := membershipModel{UserID: "subject", OrgUnitID: "org", PositionID: "position", Status: domain.StatusActive, InheritAuthorization: true, ValidFrom: &past, ValidUntil: &future}
	tests := []struct {
		name         string
		old          *membershipModel
		change       func(*membershipModel)
		scenario     auditMembershipScenario
		forbidden    bool
		queryFailure bool
		noReads      bool
	}{
		{name: "new protected role requires super", scenario: auditMembershipScenario{}, forbidden: true},
		{name: "security administrator has no membership bypass", scenario: auditMembershipScenario{operatorRole: "platform-security-admin"}, forbidden: true},
		{name: "super can delegate protected role", scenario: auditMembershipScenario{operatorRole: "platform-super-admin"}},
		{name: "ordinary role not held by operator", scenario: auditMembershipScenario{roleCode: "ordinary"}, forbidden: true},
		{name: "ordinary role held by operator", scenario: auditMembershipScenario{roleCode: "ordinary", delegated: true}},
		{name: "no inherited roles", scenario: auditMembershipScenario{noRoles: true}},
		{name: "unchanged", old: &base, noReads: true},
		{name: "shorter time window", old: &base, change: func(m *membershipModel) { m.ValidFrom = &now }, noReads: true},
		{name: "disable", change: func(m *membershipModel) { m.Status = domain.StatusDisabled }, noReads: true},
		{name: "inheritance off", change: func(m *membershipModel) { m.InheritAuthorization = false }, noReads: true},
		{name: "already expired", change: func(m *membershipModel) { m.ValidUntil = &past }, noReads: true},
		{name: "extended window checked", old: &base, change: func(m *membershipModel) { m.ValidUntil = &later }, forbidden: true},
		{name: "different user checked", old: &base, change: func(m *membershipModel) { m.UserID = "other" }, forbidden: true},
		{name: "different position checked", old: &base, change: func(m *membershipModel) { m.PositionID = "other" }, forbidden: true},
		{name: "dependency fails closed", scenario: auditMembershipScenario{queryError: true}, queryFailure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := base
			if tt.change != nil {
				tt.change(&desired)
			}
			scenario := tt.scenario
			db := auditDelegationDatabase(t, &scenario)
			// 调用方传入的过期超级角色不能替代当前数据库中的授权事实。
			ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator"}, Roles: []authctx.ReferenceName{{Code: "platform-super-admin"}}})
			err := validateMembershipDelegation(ctx, db, "tenant-1", "operator", desired, tt.old, now)
			if tt.forbidden {
				if !errors.Is(err, authzapplication.ErrForbidden) {
					t.Fatalf("error=%v, want forbidden", err)
				}
			} else if tt.queryFailure {
				if err == nil {
					t.Fatal("dependency failure accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tt.noReads && len(scenario.queries) != 0 {
				t.Fatalf("safe reduction performed %d reads", len(scenario.queries))
			}
		})
	}
}

func TestEmployeeOnboardingRejectsInheritedProtectedRolesBeforeCreatingUsers(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			scenario := &auditMembershipScenario{}
			db := auditDelegationDatabase(t, scenario)
			repo, _ := NewGORMRepository(db)
			write := application.EmployeeOnboardingWrite{User: application.UserWrite{ID: "new-user", RoleBindingID: "baseline-binding", UserCreateInput: application.UserCreateInput{TenantID: "tenant-1", OperatorID: "operator", Status: domain.StatusActive}}, MembershipID: "new-membership", Membership: &application.MembershipCreateInput{TenantID: "tenant-1", OperatorID: "operator", UserID: "new-user", OrgUnitID: "org-1", PositionID: "privileged-position", MembershipType: domain.MembershipSecondary}}
			var err error
			if batch {
				write.Account = &application.LocalAccountCreateWrite{TenantID: "tenant-1", UserID: "new-user", AccountName: "new-account"}
				_, err = repo.CreateEmployees(context.Background(), []application.EmployeeOnboardingWrite{write})
			} else {
				_, _, _, err = repo.CreateEmployee(context.Background(), write)
			}
			if !errors.Is(err, authzapplication.ErrForbidden) {
				t.Fatalf("error=%v, want forbidden before any user insertion", err)
			}
			for _, q := range scenario.queries {
				if strings.HasPrefix(q, "INSERT ") {
					t.Fatalf("unexpected partial write: %s", q)
				}
			}
		})
	}
}
func TestImportedApplicationRolesRequireOperatorDelegation(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delegated=%t", allowed), func(t *testing.T) {
			scenario := &auditMembershipScenario{delegated: allowed}
			db := auditDelegationDatabase(t, scenario)
			_, err := resolveImportedApplicationRoles(db, "tenant-1", []application.UserWrite{{UserCreateInput: application.UserCreateInput{TenantID: "tenant-1", OperatorID: "operator"}, ApplicationRoleBindings: []application.ApplicationRoleBindingWrite{{ApplicationCode: "platform", RoleCode: "ordinary"}}}})
			if allowed && err != nil {
				t.Fatal(err)
			}
			if !allowed && !errors.Is(err, authzapplication.ErrForbidden) {
				t.Fatalf("error=%v, want delegation forbidden", err)
			}
		})
	}
}

func TestUpdateMembershipRejectsEnablingInheritanceBeforeWriting(t *testing.T) {
	scenario := &auditMembershipScenario{}
	db := auditDelegationDatabase(t, scenario)
	repo, _ := NewGORMRepository(db)
	enabled := true
	status := domain.StatusActive
	_, err := repo.UpdateMembership(context.Background(), application.MembershipUpdateInput{MembershipCreateInput: application.MembershipCreateInput{TenantID: "tenant-1", OperatorID: "attacker", UserID: "attacker", OrgUnitID: "org-1", PositionID: "old-position", MembershipType: domain.MembershipSecondary, InheritAuthorization: &enabled}, MembershipID: "existing-membership", Version: 1, Status: &status})
	if !errors.Is(err, authzapplication.ErrForbidden) {
		t.Fatalf("error=%v, want forbidden", err)
	}
	if scenario.committed {
		t.Fatal("unauthorized inherited grant committed")
	}
	for _, q := range scenario.queries {
		if strings.HasPrefix(q, "UPDATE ") || strings.HasPrefix(q, "INSERT ") {
			t.Fatalf("unexpected partial write: %s", q)
		}
	}
}
func TestPersonnelChangeExecutionRechecksPersistedApproverBeforeWriting(t *testing.T) {
	scenario := &auditMembershipScenario{sourceInherit: true}
	db := auditDelegationDatabase(t, scenario)
	repo := NewPersonnelChangeGORMRepository(db)
	_, err := repo.Execute(context.Background(), application.PersonnelChangeRequest{ID: "request-1", TenantID: "tenant-1", Version: 1}, "system-worker", time.Now().UTC())
	if !errors.Is(err, authzapplication.ErrForbidden) {
		t.Fatalf("error=%v, want forbidden", err)
	}
	if scenario.authority != "approver" {
		t.Fatalf("authority=%q, want persisted approver", scenario.authority)
	}
	for _, q := range scenario.queries {
		if strings.HasPrefix(q, "UPDATE ") || strings.HasPrefix(q, "INSERT ") {
			t.Fatalf("unexpected partial write: %s", q)
		}
	}
}
