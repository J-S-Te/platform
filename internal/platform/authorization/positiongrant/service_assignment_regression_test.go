package positiongrant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// SQL 驱动不使用网络或数据库。测试调用公开服务，
// 并在首次变更时停止，以证明全部委派检查均先于任何写入。
var errAssignmentFirstWrite = errors.New("test reached first write")
var assignmentRegressionCounter uint64

type assignmentRegressionScenario struct {
	query            func(string, []driver.NamedValue) (driver.Rows, error)
	writes           int
	permissionChecks int
}
type assignmentRegressionDriver struct{ scenario *assignmentRegressionScenario }
type assignmentRegressionConn struct{ scenario *assignmentRegressionScenario }
type assignmentRegressionTx struct{}

func (*assignmentRegressionTx) Commit() error   { return nil }
func (*assignmentRegressionTx) Rollback() error { return nil }
func (d *assignmentRegressionDriver) Open(string) (driver.Conn, error) {
	return &assignmentRegressionConn{d.scenario}, nil
}
func (*assignmentRegressionConn) Close() error { return nil }
func (*assignmentRegressionConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (*assignmentRegressionConn) Begin() (driver.Tx, error) { return &assignmentRegressionTx{}, nil }
func (c *assignmentRegressionConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.scenario.writes++
	return nil, errAssignmentFirstWrite
}
func (c *assignmentRegressionConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "`iam_tenant`") {
		return regressionRows("id", []driver.Value{"tenant-1"}), nil
	}
	return c.scenario.query(strings.ToLower(q), args)
}

type assignmentRegressionClock struct{ now time.Time }

func (c assignmentRegressionClock) Now() time.Time { return c.now }

type assignmentRegressionIDs struct{}

func (assignmentRegressionIDs) New(time.Time) (string, error) { return "generated-id", nil }
func regressionDB(t *testing.T, s *assignmentRegressionScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("position-regression-%d", atomic.AddUint64(&assignmentRegressionCounter, 1))
	sql.Register(name, &assignmentRegressionDriver{s})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	result, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func regressionRows(columns string, values ...[]driver.Value) driver.Rows {
	return &delegationRows{columns: strings.Split(columns, ","), values: values}
}
func regressionTime(hour int) *time.Time {
	value := time.Date(2026, 10, 21, hour, 0, 0, 0, time.UTC)
	return &value
}
func regressionTimeValue(value *time.Time) driver.Value {
	if value == nil {
		return nil
	}
	return *value
}

func TestReplaceAssignmentsChecksDelegationBeforeAnyMutation(t *testing.T) {
	cases := []struct {
		name                                 string
		oldStatus, newStatus                 string
		omit, noBinding                      bool
		oldFrom, oldUntil, newFrom, newUntil *time.Time
		permissions                          int64
		protected, super, securityAdmin      bool
		want                                 error
		checks                               int
	}{
		{name: "new assignment rejects missing permissions", want: ErrForbidden, checks: 1},
		{name: "reactivation rejects missing permissions", oldStatus: disabledStatus, want: ErrForbidden, checks: 1},
		{name: "expanded window rejects missing permissions", oldStatus: activeStatus, oldFrom: regressionTime(9), newFrom: regressionTime(8), want: ErrForbidden, checks: 1},
		{name: "remove end rejects missing permissions", oldStatus: activeStatus, oldUntil: regressionTime(18), want: ErrForbidden, checks: 1},
		{name: "unchanged remains editable after losing permissions", oldStatus: activeStatus, want: errAssignmentFirstWrite},
		{name: "narrower window remains editable after losing permissions", oldStatus: activeStatus, newFrom: regressionTime(9), newUntil: regressionTime(18), want: errAssignmentFirstWrite},
		{name: "removal remains allowed after losing permissions", oldStatus: activeStatus, omit: true, want: errAssignmentFirstWrite},
		{name: "disable remains allowed after losing permissions", oldStatus: activeStatus, newStatus: disabledStatus, want: errAssignmentFirstWrite},
		{name: "missing materialized binding is a new grant", oldStatus: activeStatus, noBinding: true, want: ErrForbidden, checks: 1},
		{name: "future grant requires delegation now", newFrom: regressionTime(13), newUntil: regressionTime(14), want: ErrForbidden, checks: 1},
		{name: "expired only window grants no future permissions", newUntil: regressionTime(11), want: errAssignmentFirstWrite},
		{name: "owned permissions allow addition", permissions: 1, want: errAssignmentFirstWrite, checks: 1},
		{name: "authenticated super administrator exception", super: true, want: errAssignmentFirstWrite},
		{name: "authenticated security administrator exception", securityAdmin: true, want: errAssignmentFirstWrite},
		{name: "protected platform role cannot be delegated even by super administrator", protected: true, super: true, want: ErrValidation},
		{name: "protected platform role cannot be delegated even by security administrator", protected: true, securityAdmin: true, want: ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scenario := &assignmentRegressionScenario{}
			scenario.query = func(q string, args []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "from `iam_position`"):
					if strings.Contains(q, "for update") {
						return regressionRows("id", []driver.Value{"position-1"}), nil
					}
					return regressionRows("count", []driver.Value{int64(1)}), nil
				case strings.Contains(q, "authz_position_grant_template_assignment"):
					if tc.oldStatus == "" {
						return regressionRows("id"), nil
					}
					return regressionRows("id,tenant_id,position_id,template_id,status,valid_from,valid_until", []driver.Value{"old-assignment", "tenant-1", "position-1", "template-1", tc.oldStatus, regressionTimeValue(tc.oldFrom), regressionTimeValue(tc.oldUntil)}), nil
				case strings.Contains(q, "authz_position_grant_template_role"):
					return regressionRows("id,tenant_id,template_id,application_id,role_id,scope_type,scope_id,status", []driver.Value{"item-1", "tenant-1", "template-1", "app-1", "role-1", scopeTenant, "", activeStatus}), nil
				case strings.Contains(q, "authz_position_grant_template"):
					if strings.HasPrefix(q, "select `status`") || strings.HasPrefix(q, "select status ") {
						return regressionRows("status", []driver.Value{activeStatus}), nil
					}
					return regressionRows("id,status", []driver.Value{"template-1", activeStatus}), nil
				case strings.Contains(q, "authz_role as role") && !strings.Contains(q, "authz_role_binding as binding"):
					typ, code := "APPLICATION", "sales"
					if tc.protected {
						typ, code = "PLATFORM", "platform-super-admin"
					}
					return regressionRows("id,role_type,code", []driver.Value{"role-1", typ, code}), nil
				case strings.Contains(q, "authz_role_permission as role_permission") && !strings.Contains(q, "authz_role_binding as binding"):
					return regressionRows("permission_id", []driver.Value{"permission-1"}), nil
				case strings.Contains(q, "count(distinct("):
					scenario.permissionChecks++
					if scenario.writes != 0 {
						t.Fatal("delegation checked after writing")
					}
					return regressionRows("count", []driver.Value{tc.permissions}), nil
				case strings.Contains(q, "authz_role_binding"):
					if tc.oldStatus == "" || tc.noBinding {
						return regressionRows("id"), nil
					}
					return regressionRows("id,application_id,role_id,scope_type,scope_id,status,origin_item_id,valid_from,valid_until", []driver.Value{"binding-1", "app-1", "role-1", scopeTenant, "", tc.oldStatus, "item-1", regressionTimeValue(tc.oldFrom), regressionTimeValue(tc.oldUntil)}), nil
				default:
					return nil, fmt.Errorf("unexpected query: %s", q)
				}
			}
			service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{*regressionTime(12)}, ids: assignmentRegressionIDs{}}
			ctx := context.Background()
			if tc.super {
				ctx = authctx.WithPrincipal(ctx, authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"}, Roles: []authctx.ReferenceName{{Code: "platform-super-admin"}}})
			}
			if tc.securityAdmin {
				ctx = authctx.WithPrincipal(ctx, authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"}, Roles: []authctx.ReferenceName{{Code: "platform-security-admin"}}})
			}
			status := tc.newStatus
			if status == "" {
				status = activeStatus
			}
			assignments := []AssignmentInput{{TemplateID: "template-1", Status: status, ValidFrom: tc.newFrom, ValidUntil: tc.newUntil}}
			if tc.omit {
				assignments = nil
			}
			var readRows []assignmentModel
			if tc.oldStatus != "" {
				readRows = []assignmentModel{{ID: "old-assignment", TenantID: "tenant-1", PositionID: "position-1", TemplateID: "template-1", Status: tc.oldStatus, ValidFrom: tc.oldFrom, ValidUntil: tc.oldUntil}}
			}
			_, err := service.ReplacePositionAssignments(ctx, "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{Assignments: assignments, ExpectedRevision: assignmentSetRevision("tenant-1", "position-1", readRows)})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if scenario.permissionChecks != tc.checks {
				t.Errorf("delegation checks = %d, want %d", scenario.permissionChecks, tc.checks)
			}
			if !errors.Is(tc.want, errAssignmentFirstWrite) && scenario.writes != 0 {
				t.Fatalf("unauthorized save performed %d writes", scenario.writes)
			}
		})
	}
}

func TestPreviewIntersectsTemplateValidity(t *testing.T) {
	for _, tc := range []struct {
		name                string
		from, until         *time.Time
		wantRoles           int
		wantFrom, wantUntil *time.Time
	}{
		{"template narrows otherwise unlimited authorization", regressionTime(10), regressionTime(11), 1, regressionTime(10), regressionTime(11)},
		{"disjoint template produces no role or conflict", regressionTime(13), regressionTime(14), 0, nil, nil},
		{"touching boundaries produce no role", regressionTime(12), regressionTime(13), 0, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &assignmentRegressionScenario{}
			s.query = func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "iam_position"):
					if strings.Contains(q, "for update") {
						return regressionRows("id", []driver.Value{"position-1"}), nil
					}
					return regressionRows("count", []driver.Value{int64(1)}), nil
				case strings.Contains(q, "authz_position_grant_template_assignment"):
					if !strings.Contains(q, "template.valid_from as template_from") || !strings.Contains(q, "template.valid_until as template_until") {
						t.Error("preview SQL must select template validity")
					}
					return regressionRows("template_id,template_name,application_id,role_id,template_from,template_until", []driver.Value{"template-1", "Standard", "app-1", "role-1", regressionTimeValue(tc.from), regressionTimeValue(tc.until)}), nil
				case strings.Contains(q, "authz_application_authorization_policy"):
					return regressionRows("application_id,max_effective_roles"), nil
				default:
					return nil, fmt.Errorf("unexpected query: %s", q)
				}
			}
			service := &Service{db: regressionDB(t, s), clock: assignmentRegressionClock{*regressionTime(12)}}
			got, err := service.Preview(context.Background(), "tenant-1", PreviewInput{PositionID: "position-1", EffectiveFrom: regressionTime(9), EffectiveTo: regressionTime(12)})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Roles) != tc.wantRoles {
				t.Fatalf("roles = %#v, want count %d", got.Roles, tc.wantRoles)
			}
			if len(got.Roles) > 0 && (!sameTime(got.Roles[0].ValidFrom, tc.wantFrom) || !sameTime(got.Roles[0].ValidUntil, tc.wantUntil)) {
				t.Fatalf("role window = %v/%v, want %v/%v", got.Roles[0].ValidFrom, got.Roles[0].ValidUntil, tc.wantFrom, tc.wantUntil)
			}
			if len(got.Conflicts) != 0 {
				t.Fatalf("unexpected conflicts: %v", got.Conflicts)
			}
		})
	}
}

func TestAffectedUsersQueryRequiresEffectivePositionBindings(t *testing.T) {
	// 此处验证发给驱动的 SQL 及其响应映射，
	// 不声称已在真实数据库中执行 MySQL 谓词。
	scenario := &assignmentRegressionScenario{}
	var countQuery string
	var countArgs []driver.NamedValue
	scenario.query = func(q string, args []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "authz_position_grant_template_role") && !strings.Contains(q, "iam_membership"):
			return regressionRows("id"), nil
		case strings.Contains(q, "from `authz_position_grant_template`"):
			return regressionRows("id,status", []driver.Value{"template-1", activeStatus}), nil
		case strings.Contains(q, "from `authz_position_grant_template_assignment`"):
			return regressionRows("count", []driver.Value{int64(3)}), nil
		case strings.Contains(q, "iam_membership"):
			countQuery, countArgs = q, args
			if strings.Contains(q, "for update") {
				return regressionRows("id", []driver.Value{"position-1"}), nil
			}
			return regressionRows("count", []driver.Value{int64(1)}), nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	now := *regressionTime(12)
	service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{now}}
	got, err := service.Get(context.Background(), "tenant-1", "template-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AffectedUsers != 1 || got.AssignmentCount != 3 {
		t.Fatalf("view = %#v", got)
	}
	for _, required := range []string{
		"authz_role_binding as binding", "binding.subject_type", "binding.subject_id", "binding.grant_origin", "binding.origin_id", "binding.origin_item_id", "binding.status", "binding.valid_from", "binding.valid_until",
		"iam_user as user", "user.status", "user.valid_until", "iam_org_unit as organization", "organization.status", "iam_position as position", "position.status", "position.org_unit_id",
		"m.inherit_authorization", "m.valid_from", "m.valid_until", "a.valid_from", "a.valid_until", "template.status", "template.valid_from", "template.valid_until", "item.status", "item.valid_from", "item.valid_until",
	} {
		if !strings.Contains(countQuery, required) {
			t.Errorf("affected-user SQL is missing %q: %s", required, countQuery)
		}
	}
	foundNow := false
	for _, arg := range countArgs {
		if value, ok := arg.Value.(time.Time); ok && value.Equal(now) {
			foundNow = true
		}
	}
	if !foundNow {
		t.Error("affected-user count did not use service clock")
	}
}

func TestReplaceAssignmentsChecksWholeBatchBeforeWrites(t *testing.T) {
	for _, removeExisting := range []bool{false, true} {
		t.Run(fmt.Sprintf("remove-existing-%t", removeExisting), func(t *testing.T) {
			scenario := &assignmentRegressionScenario{}
			requestedRole := ""
			scenario.query = func(q string, args []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "from `iam_position`"):
					if strings.Contains(q, "for update") {
						return regressionRows("id", []driver.Value{"position-1"}), nil
					}
					return regressionRows("count", []driver.Value{int64(1)}), nil
				case strings.Contains(q, "authz_position_grant_template_assignment"):
					if removeExisting {
						return regressionRows("id,template_id,status", []driver.Value{"old-assignment", "template-removed", activeStatus}), nil
					}
					return regressionRows("id"), nil
				case strings.Contains(q, "authz_position_grant_template_role"):
					id := args[1].Value.(string)
					return regressionRows("id,application_id,role_id,scope_type,status", []driver.Value{"item-" + id, "app-1", "role-" + id, scopeTenant, activeStatus}), nil
				case strings.Contains(q, "authz_position_grant_template"):
					return regressionRows("id,status", []driver.Value{args[1].Value.(string), activeStatus}), nil
				case strings.Contains(q, "authz_role as role") && !strings.Contains(q, "authz_role_binding as binding"):
					return regressionRows("role_type,code", []driver.Value{roleTypeApplication, "sales"}), nil
				case strings.Contains(q, "authz_role_permission as role_permission") && !strings.Contains(q, "authz_role_binding as binding"):
					requestedRole = args[3].Value.(string)
					return regressionRows("permission_id", []driver.Value{"permission-1"}), nil
				case strings.Contains(q, "count(distinct("):
					scenario.permissionChecks++
					var count int64
					if requestedRole == "role-template-first" {
						count = 1
					}
					return regressionRows("count", []driver.Value{count}), nil
				default:
					return nil, fmt.Errorf("unexpected query: %s", q)
				}
			}
			service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{*regressionTime(12)}, ids: assignmentRegressionIDs{}}
			var readRows []assignmentModel
			if removeExisting {
				readRows = []assignmentModel{{ID: "old-assignment", TemplateID: "template-removed", Status: activeStatus}}
			}
			_, err := service.ReplacePositionAssignments(context.Background(), "tenant-1", "operator-1", "position-1", ReplaceAssignmentsInput{Assignments: []AssignmentInput{{TemplateID: "template-first"}, {TemplateID: "template-second"}}, ExpectedRevision: assignmentSetRevision("tenant-1", "position-1", readRows)})
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("error = %v, want ErrForbidden", err)
			}
			if scenario.permissionChecks != 2 || scenario.writes != 0 {
				t.Fatalf("checks/writes = %d/%d, want 2/0", scenario.permissionChecks, scenario.writes)
			}
		})
	}
}

func TestPreviewUsesAllFourValidityWindows(t *testing.T) {
	scenario := &assignmentRegressionScenario{}
	scenario.query = func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "iam_position"):
			if strings.Contains(q, "for update") {
				return regressionRows("id", []driver.Value{"position-1"}), nil
			}
			return regressionRows("count", []driver.Value{int64(1)}), nil
		case strings.Contains(q, "authz_position_grant_template_assignment"):
			return regressionRows("template_id,application_id,role_id,template_from,template_until,assignment_from,assignment_until,item_from,item_until", []driver.Value{"template-1", "app-1", "role-1", *regressionTime(10), *regressionTime(17), *regressionTime(11), *regressionTime(16), *regressionTime(12), *regressionTime(15)}), nil
		case strings.Contains(q, "authz_application_authorization_policy"):
			return regressionRows("application_id,max_effective_roles"), nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	service := &Service{db: regressionDB(t, scenario), clock: assignmentRegressionClock{*regressionTime(12)}}
	got, err := service.Preview(context.Background(), "tenant-1", PreviewInput{PositionID: "position-1", EffectiveFrom: regressionTime(9), EffectiveTo: regressionTime(18)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 1 || !sameTime(got.Roles[0].ValidFrom, regressionTime(12)) || !sameTime(got.Roles[0].ValidUntil, regressionTime(15)) {
		t.Fatalf("preview = %#v, want role valid 12:00–15:00", got)
	}
}
