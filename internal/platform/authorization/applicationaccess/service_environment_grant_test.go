package applicationaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 仅替换 SQL 输入输出；环境查询、有效范围筛选、
// 角色策略及最终 GRANTED 判定均执行生产代码。
type environmentGrantScenario struct {
	scope, subject string
	roleCount      int
	maximum        int
	superAdmin     bool
	failLookup     bool
	queryCount     *atomic.Int32
}

type environmentGrantDriver struct{ scenario environmentGrantScenario }
type environmentGrantConn struct{ scenario environmentGrantScenario }
type environmentGrantRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *environmentGrantRows) Columns() []string { return r.columns }
func (*environmentGrantRows) Close() error        { return nil }
func (r *environmentGrantRows) Next(out []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(out, r.values[r.index])
	r.index++
	return nil
}
func (d environmentGrantDriver) Open(string) (driver.Conn, error) {
	return &environmentGrantConn{scenario: d.scenario}, nil
}
func (*environmentGrantConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared query")
}
func (*environmentGrantConn) Close() error { return nil }

// readBoundaryTx 允许管理端 GET 为"可编辑集合与版本同快照"开启只读事务；
// 事务内任何写入仍被 ExecContext 拒绝（该桩未实现 Exec，database/sql 对
// 未实现 ExecContext 的 Conn 走 Prepare 路径，同样被拒绝）。
type environmentGrantReadTx struct{}

func (environmentGrantReadTx) Commit() error   { return nil }
func (environmentGrantReadTx) Rollback() error { return nil }

func (*environmentGrantConn) Begin() (driver.Tx, error) {
	return environmentGrantReadTx{}, nil
}
func environmentGrantArg(args []driver.NamedValue, want string) bool {
	for _, arg := range args {
		if arg.Value == want {
			return true
		}
	}
	return false
}
func (c *environmentGrantConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.scenario.queryCount != nil {
		c.scenario.queryCount.Add(1)
	}
	q := strings.ReplaceAll(strings.ToLower(query), "`", "")
	has := func(v string) bool { return environmentGrantArg(args, v) }
	switch {
	case strings.Contains(q, "order by rb.id"):
		// edit_revision 的绑定行版本查询：必须带租户/应用/主体来源边界。
		if !has("tenant-1") || !has("app-1") || !has("MANUAL") {
			return nil, errors.New("edit revision query lost tenant/application/origin boundary")
		}
		return &environmentGrantRows{columns: []string{"id", "version", "role_id", "scope_type", "scope_id", "status", "valid_from", "valid_until", "grant_origin", "origin_id", "origin_item_id"}}, nil
	case strings.Contains(q, "authz_user_permission"):
		// edit_revision 的历史自定义权限查询：必须带租户/应用边界。
		if !has("tenant-1") || !has("app-1") {
			return nil, errors.New("edit revision permission query lost tenant/application boundary")
		}
		return &environmentGrantRows{columns: []string{"permission_id", "created_at"}}, nil
	case strings.Contains(q, "from platform_application "):
		rows := &environmentGrantRows{columns: []string{"id", "tenant_id", "code"}}
		if has("tenant-1") && (has("orders") || has("app-1")) && has("ACTIVE") {
			rows.values = [][]driver.Value{{"app-1", "tenant-1", "orders"}}
		}
		return rows, nil
	case strings.Contains(q, "from platform_application_environment "):
		if c.scenario.failLookup {
			return nil, errors.New("environment store unavailable")
		}
		if !strings.Contains(q, "tenant_id = ?") || !strings.Contains(q, "application_id = ?") || !strings.Contains(q, "environment = ?") || !strings.Contains(q, "status = ?") || !has("tenant-1") || !has("app-1") || !has("ACTIVE") {
			return nil, fmt.Errorf("environment lookup lost tenant/application/active boundary: %s", query)
		}
		rows := &environmentGrantRows{columns: []string{"id"}}
		if has("dev") {
			rows.values = [][]driver.Value{{"env-dev"}}
		} else if has("prod") {
			rows.values = [][]driver.Value{{"env-prod"}}
		}
		return rows, nil
	case strings.Contains(q, "from authz_role_binding as rb"):
		if !has("tenant-1") || !has("app-1") || !has("ACTIVE") || (!has("MANUAL") && (!strings.Contains(q, "rb.valid_from <=") || !strings.Contains(q, "rb.valid_until >"))) {
			return nil, errors.New("effective role query lost tenant/application/time boundary")
		}
		rows := &environmentGrantRows{columns: []string{"role_id", "code", "scope_type", "scope_id", "environment_code", "subject_type", "subject_id"}}
		if !has(c.scenario.subject) || (c.scenario.scope == "ENVIRONMENT" && has("env-prod")) {
			return rows, nil
		}
		for i := 0; i < c.scenario.roleCount; i++ {
			scopeID, code := "", ""
			if c.scenario.scope == "ENVIRONMENT" {
				scopeID, code = "env-dev", "dev"
			}
			rows.values = append(rows.values, []driver.Value{fmt.Sprintf("role-%d", i), fmt.Sprintf("reader-%d", i), c.scenario.scope, scopeID, code, "USER", c.scenario.subject})
		}
		return rows, nil
	case strings.Contains(q, "from authz_role_binding as binding"):
		count := int64(0)
		if c.scenario.superAdmin {
			count = 1
		}
		return &environmentGrantRows{columns: []string{"count"}, values: [][]driver.Value{{count}}}, nil
	case strings.Contains(q, "from authz_role "):
		return &environmentGrantRows{columns: []string{"role_id", "code", "name"}, values: [][]driver.Value{{"role-admin", "admin", "管理员"}}}, nil
	case strings.Contains(q, "from authz_application_authorization_policy"):
		return &environmentGrantRows{columns: []string{"application_id", "max_effective_roles"}, values: [][]driver.Value{{"app-1", int64(c.scenario.maximum)}}}, nil
	case strings.Contains(q, "from authz_role_permission as rp"):
		return &environmentGrantRows{columns: []string{"code"}, values: [][]driver.Value{{"orders.read"}}}, nil
	case strings.Contains(q, "from authz_authorization_catalog"):
		return &environmentGrantRows{columns: []string{"claims_role_config_hash"}, values: [][]driver.Value{{"test-hash"}}}, nil
	case strings.Contains(q, "from authz_policy_revision"):
		return &environmentGrantRows{columns: []string{"revision"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(q, "from iam_membership as membership"):
		if !has("tenant-1") || !has("user-1") {
			return nil, errors.New("organization query lost tenant/identity boundary")
		}
		return &environmentGrantRows{columns: []string{"organization_id", "is_primary"}, values: [][]driver.Value{{"org-1", true}}}, nil
	case strings.Contains(q, "from iam_user"):
		if !has("tenant-1") || !has("user-1") {
			return nil, errors.New("person query lost tenant/identity boundary")
		}
		return &environmentGrantRows{columns: []string{"pms_person_id"}, values: [][]driver.Value{{"person-1"}}}, nil
	default:
		return nil, fmt.Errorf("unexpected environment grant query: %s", query)
	}
}

var environmentGrantDriverID uint64

func newEnvironmentGrantService(t *testing.T, scenario environmentGrantScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("environment-grant-%d", atomic.AddUint64(&environmentGrantDriverID, 1))
	sql.Register(name, environmentGrantDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{db: db, clock: SystemClock{}}
}

func TestHasApplicationEnvironmentGrantChecksTargetEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, user, environment string
		scenario                        environmentGrantScenario
		want                            bool
		wantErr                         bool
	}{
		{"dev_only_dev", "tenant-1", "user-1", "dev", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1}, true, false},
		{"dev_only_prod", "tenant-1", "user-1", "prod", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1}, false, false},
		{"tenant_scope_prod", "tenant-1", "user-1", "prod", environmentGrantScenario{scope: "TENANT", subject: "user-1", roleCount: 1}, true, false},
		{"other_tenant", "tenant-2", "user-1", "dev", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1}, false, false},
		{"other_user", "tenant-1", "user-2", "dev", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1}, false, false},
		{"unknown_environment", "tenant-1", "user-1", "missing", environmentGrantScenario{scope: "TENANT", subject: "user-1", roleCount: 1}, false, false},
		{"inactive_environment", "tenant-1", "user-1", "disabled", environmentGrantScenario{scope: "TENANT", subject: "user-1", roleCount: 1}, false, false},
		{"blank_environment", "tenant-1", "user-1", " ", environmentGrantScenario{scope: "TENANT", subject: "user-1", roleCount: 1}, false, true},
		{"no_grant", "tenant-1", "user-1", "dev", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1"}, false, false},
		{"policy_conflict", "tenant-1", "user-1", "dev", environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 2, maximum: 1}, false, false},
		{"super_admin_prod", "tenant-1", "user-1", "prod", environmentGrantScenario{superAdmin: true}, true, false},
		{"environment_query_failure", "tenant-1", "user-1", "dev", environmentGrantScenario{failLookup: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newEnvironmentGrantService(t, tc.scenario)
			got, err := svc.HasApplicationEnvironmentGrant(context.Background(), tc.tenant, tc.user, "orders", tc.environment)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("grant=%v err=%v; want grant=%v error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestEnvironmentGrantKeepsManagementAccessAggregated(t *testing.T) {
	svc := newEnvironmentGrantService(t, environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1})
	access, err := svc.GetAccess(context.Background(), "tenant-1", "user-1", "orders")
	if err != nil || access.AuthorizationState != "GRANTED" || len(access.Roles) != 1 || access.Roles[0].EnvironmentCode != "dev" {
		t.Fatalf("management aggregate access=%+v err=%v", access, err)
	}
	granted, err := svc.HasApplicationGrant(context.Background(), "tenant-1", "user-1", "orders")
	if err != nil || !granted {
		t.Fatalf("legacy application grant=%v err=%v", granted, err)
	}
	granted, err = svc.HasApplicationEnvironmentGrant(context.Background(), "tenant-1", "user-1", "orders", "prod")
	if err != nil || granted {
		t.Fatalf("prod environment grant=%v err=%v", granted, err)
	}
}

func TestEnvironmentGrantRejectsMissingTargetsAndNormalizesInput(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, user, application, environment string
		want, wantErr                                bool
	}{
		{"missing_tenant", "", "user-1", "orders", "dev", false, true},
		{"missing_user", "tenant-1", "", "orders", "dev", false, true},
		{"missing_application", "tenant-1", "user-1", "", "dev", false, true},
		{"unknown_application", "tenant-1", "user-1", "other-app", "dev", false, false},
		{"trimmed_targets", " tenant-1 ", " user-1 ", " orders ", " dev ", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newEnvironmentGrantService(t, environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1})
			got, err := svc.HasApplicationEnvironmentGrant(context.Background(), tc.tenant, tc.user, tc.application, tc.environment)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("grant=%v err=%v; want grant=%v error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestKeycloakSnapshotClearsRolesAndPermissionsWhenNotGranted(t *testing.T) {
	for _, tc := range []struct {
		name            string
		count, maximum  int
		wantRoles       int
		wantPermissions int
	}{
		{"GRANTED", 1, 1, 1, 1},
		{"CONFLICT", 2, 1, 0, 0},
		{"UNAUTHORIZED", 0, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newEnvironmentGrantService(t, environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: tc.count, maximum: tc.maximum})
			snapshot, err := svc.ResolveKeycloakAuthorizationSnapshot(context.Background(), "tenant-1", "user-1", "app-1", "env-dev")
			if err != nil {
				t.Fatalf("snapshot must permit reconciling denied authorization to an empty projection: %v", err)
			}
			if len(snapshot.Roles) != tc.wantRoles || len(snapshot.Permissions) != tc.wantPermissions {
				t.Fatalf("roles=%v permissions=%v; want counts %d/%d", snapshot.Roles, snapshot.Permissions, tc.wantRoles, tc.wantPermissions)
			}
			if tc.name == "GRANTED" && (snapshot.Roles[0] != "reader-0" || snapshot.Permissions[0] != "orders.read") {
				t.Fatalf("effective role/permission changed: %+v", snapshot)
			}
			if snapshot.TenantID != "tenant-1" || snapshot.IdentityID != "user-1" || snapshot.ApplicationID != "app-1" || snapshot.ApplicationCode != "orders" || snapshot.PersonID != "person-1" || snapshot.PrimaryOrgID != "org-1" || len(snapshot.OrganizationIDs) != 1 || snapshot.OrganizationIDs[0] != "org-1" || snapshot.RoleConfigHash != "test-hash" || snapshot.AuthzRevision != 1 {
				t.Fatalf("authorization context must be retained for reconciliation: %+v", snapshot)
			}
		})
	}
}

func TestKeycloakSnapshotRejectsMissingEnvironmentBeforeDatabase(t *testing.T) {
	for _, environmentID := range []string{"", " \t\n"} {
		t.Run(fmt.Sprintf("environment_%q", environmentID), func(t *testing.T) {
			var queryCount atomic.Int32
			svc := newEnvironmentGrantService(t, environmentGrantScenario{scope: "ENVIRONMENT", subject: "user-1", roleCount: 1, queryCount: &queryCount})
			_, err := svc.ResolveKeycloakAuthorizationSnapshot(context.Background(), "tenant-1", "user-1", "app-1", environmentID)
			if !errors.Is(err, ErrValidation) || queryCount.Load() != 0 {
				t.Fatalf("err=%v queries=%d; want validation before any database access", err, queryCount.Load())
			}
		})
	}
}
