package positiongrant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAuthorizationTargetViewsExposePlatformNativeRolesAsReady(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID:   "platform-app",
			ApplicationCode: "platform",
			ApplicationName: "基础能力平台",
			RoleID:          "role-security-admin",
			RoleCode:        "platform-security-admin",
			RoleName:        "平台安全管理员",
			RoleType:        roleTypePlatform,
			RoleStatus:      activeStatus,
		},
	}, false)

	want := []AuthorizationTargetView{
		{
			ApplicationID:    "platform-app",
			ApplicationCode:  "platform",
			ApplicationName:  "基础能力平台",
			CatalogVersion:   "built-in",
			CatalogSyncState: catalogStatusSynced,
			Roles: []AuthorizationTargetRoleView{
				{
					RoleID:     "role-security-admin",
					RoleCode:   "platform-security-admin",
					RoleName:   "平台安全管理员",
					RoleType:   roleTypePlatform,
					RoleStatus: activeStatus,
					Assignable: true,
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorizationTargetViews() = %#v, want %#v", got, want)
	}
}

func TestAuthorizationTargetViewsExcludeProtectedPlatformRoles(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID: "platform-app", ApplicationCode: "platform", ApplicationName: "基础能力平台",
			RoleID: "role-super-admin", RoleCode: "platform-super-admin", RoleName: "平台超级管理员",
			RoleType: roleTypePlatform, RoleStatus: activeStatus,
		},
		{
			ApplicationID: "platform-app", ApplicationCode: "platform", ApplicationName: "基础能力平台",
			RoleID: "role-security-admin", RoleCode: "platform-security-admin", RoleName: "平台安全管理员",
			RoleType: roleTypePlatform, RoleStatus: activeStatus,
		},
	}, false)

	if len(got) != 1 || len(got[0].Roles) != 1 || got[0].Roles[0].RoleCode != "platform-security-admin" {
		t.Fatalf("authorizationTargetViews() = %#v, want only the non-protected platform role", got)
	}
}

func TestAuthorizationTargetViewsPreserveSynchronizedSubsystemCatalogMetadata(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID:    "contract-app",
			ApplicationCode:  "contract_management",
			ApplicationName:  "合同管理系统",
			CatalogVersion:   "2026.07",
			CatalogSyncState: catalogStatusSynced,
			RoleID:           "role-sales",
			RoleCode:         "sales",
			RoleName:         "销售人员",
			RoleType:         roleTypeApplication,
			RoleStatus:       activeStatus,
		},
	}, false)

	if len(got) != 1 {
		t.Fatalf("target count = %d, want 1", len(got))
	}
	if got[0].CatalogVersion != "2026.07" || got[0].CatalogSyncState != catalogStatusSynced {
		t.Fatalf("catalog metadata = %q/%q", got[0].CatalogVersion, got[0].CatalogSyncState)
	}
	if len(got[0].Roles) != 1 || got[0].Roles[0].RoleType != roleTypeApplication || !got[0].Roles[0].Assignable {
		t.Fatalf("unexpected roles: %#v", got[0].Roles)
	}
}

// 子系统 CRM 的 customer_all_viewer 角色含 customer.read.all（grant_policy=
// SUPER_ADMIN_ONLY）。这类角色仍然出现在模板目标列表里，但只对租户级平台超级管理员
// 标记 assignable=true：普通操作者看不到它可选，超管仍能照常配置。

func TestAuthorizationTargetViewsMarkRestrictedRolesUnassignableForNonSuperAdmin(t *testing.T) {
	t.Parallel()

	rows := []authorizationTargetRow{{
		ApplicationID: "crm-app", ApplicationCode: "customer_and_opportunity", ApplicationName: "CRM",
		CatalogVersion: "2026.11.05", CatalogSyncState: catalogStatusSynced,
		RoleID: "role-customer-all-viewer", RoleCode: "customer_all_viewer", RoleName: "全部客户查看者",
		RoleType: roleTypeApplication, RoleStatus: activeStatus, GrantRestricted: true,
	}}

	got := authorizationTargetViews(rows, false)
	if len(got) != 1 || len(got[0].Roles) != 1 {
		t.Fatalf("authorizationTargetViews() = %#v, want one restricted role", got)
	}
	if got[0].Roles[0].RoleCode != "customer_all_viewer" || got[0].Roles[0].Assignable {
		t.Fatalf("restricted role must stay visible but not assignable for a non-super administrator: %#v", got[0].Roles)
	}

	superAdmin := authorizationTargetViews(rows, true)
	if len(superAdmin) != 1 || len(superAdmin[0].Roles) != 1 || !superAdmin[0].Roles[0].Assignable {
		t.Fatalf("restricted role must be assignable for a super administrator: %#v", superAdmin)
	}
}

func TestAuthorizationTargetViewsKeepUnrestrictedRolesAssignable(t *testing.T) {
	t.Parallel()

	got := authorizationTargetViews([]authorizationTargetRow{
		{
			ApplicationID: "crm-app", ApplicationCode: "customer_and_opportunity", ApplicationName: "CRM",
			CatalogVersion: "2026.11.05", CatalogSyncState: catalogStatusSynced,
			RoleID: "role-customer-viewer", RoleCode: "customer_viewer", RoleName: "客户查看者",
			RoleType: roleTypeApplication, RoleStatus: activeStatus,
		},
		{
			ApplicationID: "crm-app", ApplicationCode: "customer_and_opportunity", ApplicationName: "CRM",
			CatalogVersion: "2026.11.05", CatalogSyncState: catalogStatusSynced,
			RoleID: "role-customer-all-viewer", RoleCode: "customer_all_viewer", RoleName: "全部客户查看者",
			RoleType: roleTypeApplication, RoleStatus: activeStatus, GrantRestricted: true,
		},
	}, false)

	if len(got) != 1 || len(got[0].Roles) != 2 {
		t.Fatalf("authorizationTargetViews() = %#v, want both roles", got)
	}
	assignable := map[string]bool{}
	for _, role := range got[0].Roles {
		assignable[role.RoleCode] = role.Assignable
	}
	if !assignable["customer_viewer"] {
		t.Fatalf("unrestricted role must stay assignable: %#v", got[0].Roles)
	}
	if assignable["customer_all_viewer"] {
		t.Fatalf("restricted role must not be assignable for a non-super administrator: %#v", got[0].Roles)
	}
}

func TestPlatformSuperAdminFromContextOnlyAcceptsTenantLevelPrincipal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{name: "no authenticated principal", ctx: context.Background(), want: false},
		{
			name: "ordinary operator",
			ctx: authctx.WithPrincipal(context.Background(), authctx.Principal{
				Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"},
				Roles: []authctx.ReferenceName{{Code: "platform-role-admin"}},
			}),
			want: false,
		},
		{
			name: "security administrator is not a super administrator",
			ctx: authctx.WithPrincipal(context.Background(), authctx.Principal{
				Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"},
				Roles: []authctx.ReferenceName{{Code: domain.SecurityAdminRoleCode}},
			}),
			want: false,
		},
		{
			name: "super administrator from another tenant",
			ctx: authctx.WithPrincipal(context.Background(), authctx.Principal{
				Tenant: authctx.ReferenceName{ID: "tenant-2"}, User: authctx.ReferenceName{ID: "operator-1"},
				Roles: []authctx.ReferenceName{{Code: domain.SuperAdminRoleCode}},
			}),
			want: false,
		},
		{
			name: "tenant level super administrator",
			ctx: authctx.WithPrincipal(context.Background(), authctx.Principal{
				Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"},
				Roles: []authctx.ReferenceName{{Code: domain.SuperAdminRoleCode}},
			}),
			want: true,
		},
	}
	for _, testCase := range cases {
		if got := platformSuperAdminFromContext(testCase.ctx, "tenant-1"); got != testCase.want {
			t.Fatalf("%s: platformSuperAdminFromContext() = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

type authorizationTargetScenario struct {
	restricted bool
	queries    []string
	args       [][]driver.NamedValue
}

type authorizationTargetRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *authorizationTargetRows) Columns() []string { return rows.columns }
func (*authorizationTargetRows) Close() error           { return nil }

func (rows *authorizationTargetRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type authorizationTargetDriver struct{ scenario *authorizationTargetScenario }
type authorizationTargetConn struct{ scenario *authorizationTargetScenario }
type authorizationTargetTx struct{}

func (authorizationTargetTx) Commit() error   { return nil }
func (authorizationTargetTx) Rollback() error { return nil }

func (d *authorizationTargetDriver) Open(string) (driver.Conn, error) {
	return &authorizationTargetConn{scenario: d.scenario}, nil
}
func (*authorizationTargetConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*authorizationTargetConn) Close() error              { return nil }
func (*authorizationTargetConn) Begin() (driver.Tx, error) { return authorizationTargetTx{}, nil }
func (*authorizationTargetConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return authorizationTargetTx{}, nil
}
func (*authorizationTargetConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, errors.New("unexpected write")
}
func (c *authorizationTargetConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	c.scenario.args = append(c.scenario.args, args)
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	if !strings.Contains(normalized, "from platform_application as application") {
		return &authorizationTargetRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	}
	restricted := int64(0)
	if c.scenario.restricted {
		restricted = 1
	}
	return &authorizationTargetRows{
		columns: []string{"application_id", "application_code", "application_name", "catalog_version", "catalog_sync_status", "max_effective_roles", "role_id", "role_code", "role_name", "role_type", "role_status", "grant_restricted"},
		values: [][]driver.Value{{
			"crm-app", "customer_and_opportunity", "CRM", "2026.11.05", catalogStatusSynced, int64(0),
			"role-customer-all-viewer", "customer_all_viewer", "全部客户查看者", roleTypeApplication, activeStatus, restricted,
		}},
	}, nil
}

var authorizationTargetDriverCounter uint64

func newAuthorizationTargetService(t *testing.T, scenario *authorizationTargetScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("positiongrant-targets-%d", atomic.AddUint64(&authorizationTargetDriverCounter, 1))
	sql.Register(name, &authorizationTargetDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open scripted target database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open scripted gorm database: %v", err)
	}
	return &Service{db: database}
}

func targetOperatorContext(tenantID, userID string, roleCodes ...string) context.Context {
	roles := make([]authctx.ReferenceName, 0, len(roleCodes))
	for _, code := range roleCodes {
		roles = append(roles, authctx.ReferenceName{Code: code})
	}
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: tenantID}, User: authctx.ReferenceName{ID: userID}, Roles: roles,
	})
}

func TestListAuthorizationTargetsBindsGrantPolicyEnumeration(t *testing.T) {
	scenario := &authorizationTargetScenario{restricted: true}
	service := newAuthorizationTargetService(t, scenario)

	if _, err := service.ListAuthorizationTargets(targetOperatorContext("tenant-1", "operator-1", "platform-role-admin"), "tenant-1"); err != nil {
		t.Fatalf("ListAuthorizationTargets returned error: %v", err)
	}
	if len(scenario.queries) != 1 {
		t.Fatalf("expected one target query, got %#v", scenario.queries)
	}
	query := scenario.queries[0]
	normalized := strings.ToLower(query)
	if !strings.Contains(normalized, "as grant_restricted") {
		t.Fatalf("target query must expose the restricted flag: %s", query)
	}
	if !strings.Contains(normalized, "gp.grant_policy = ?") {
		t.Fatalf("target query must bind the grant policy instead of inlining it: %s", query)
	}
	if strings.Contains(normalized, "gp.grant_policy = '") {
		t.Fatalf("target query must not inline the grant policy literal: %s", query)
	}
	bound := make([]string, 0, len(scenario.args[0]))
	for _, arg := range scenario.args[0] {
		if value, ok := arg.Value.(string); ok {
			bound = append(bound, value)
		}
	}
	// Select 的参数必须排在 JOIN/WHERE 参数之前，否则占位符与参数会错位。
	if len(bound) == 0 || bound[0] != domain.GrantPolicySuperAdminOnly {
		t.Fatalf("grant policy must be the first bound value, got %#v", bound)
	}
}

func TestListAuthorizationTargetsHidesRestrictedRoleFromNonSuperAdmin(t *testing.T) {
	service := newAuthorizationTargetService(t, &authorizationTargetScenario{restricted: true})

	got, err := service.ListAuthorizationTargets(targetOperatorContext("tenant-1", "operator-1", "platform-role-admin"), "tenant-1")
	if err != nil {
		t.Fatalf("ListAuthorizationTargets returned error: %v", err)
	}
	if len(got) != 1 || len(got[0].Roles) != 1 {
		t.Fatalf("ListAuthorizationTargets() = %#v, want one restricted role", got)
	}
	if got[0].Roles[0].RoleCode != "customer_all_viewer" || got[0].Roles[0].Assignable {
		t.Fatalf("restricted role must not be assignable for a non-super administrator: %#v", got[0].Roles)
	}
}

func TestListAuthorizationTargetsShowsRestrictedRoleToSuperAdmin(t *testing.T) {
	service := newAuthorizationTargetService(t, &authorizationTargetScenario{restricted: true})

	got, err := service.ListAuthorizationTargets(targetOperatorContext("tenant-1", "operator-1", domain.SuperAdminRoleCode), "tenant-1")
	if err != nil {
		t.Fatalf("ListAuthorizationTargets returned error: %v", err)
	}
	if len(got) != 1 || len(got[0].Roles) != 1 || !got[0].Roles[0].Assignable {
		t.Fatalf("super administrator must see the restricted role as assignable: %#v", got)
	}
}

func TestListAuthorizationTargetsKeepsDefaultRolesAssignable(t *testing.T) {
	service := newAuthorizationTargetService(t, &authorizationTargetScenario{restricted: false})

	got, err := service.ListAuthorizationTargets(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("ListAuthorizationTargets returned error: %v", err)
	}
	if len(got) != 1 || len(got[0].Roles) != 1 || !got[0].Roles[0].Assignable {
		t.Fatalf("roles without restricted permissions must stay assignable: %#v", got)
	}
}
