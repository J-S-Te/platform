package applicationaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	authzdomain "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 子系统 CRM 的 customer.read.all 使用 grant_policy=SUPER_ADMIN_ONLY，并新增
// customer_all_viewer 角色。平台侧需要：
//   * 目录契约接受并校验该元数据，且把它纳入 canonical 摘要（同版本不同策略必须
//     得到不同摘要，否则订阅方无法感知授权边界的变化）；
//   * 未声明该字段时摘要保持"未声明"语义，入库才回落 DEFAULT；
//   * 目录视图对含受限权限的角色只在租户级超管视角下标记 assignable=true。

func TestNormalizeCatalogAcceptsGrantPolicyMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		submit string
		want   string
	}{
		{name: "unset stays unset", submit: "", want: ""},
		{name: "default", submit: "DEFAULT", want: authzdomain.GrantPolicyDefault},
		{name: "super administrator only", submit: "SUPER_ADMIN_ONLY", want: authzdomain.GrantPolicySuperAdminOnly},
		{name: "case and whitespace normalized", submit: "  super_admin_only  ", want: authzdomain.GrantPolicySuperAdminOnly},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			normalized, checksum, err := normalizeCatalog(CatalogInput{
				CatalogVersion: "2026.11.05",
				Permissions: []PermissionInput{{
					Code: "customer.read.all", Name: "查看全部客户", Action: "read", ResourceCode: "customer",
					GrantPolicy: testCase.submit,
				}},
			})
			if err != nil {
				t.Fatalf("grant_policy %q must be accepted: %v", testCase.submit, err)
			}
			if got := normalized.Permissions[0].GrantPolicy; got != testCase.want {
				t.Fatalf("normalized grant_policy = %q, want %q", got, testCase.want)
			}
			if checksum == "" {
				t.Fatal("checksum must not be empty")
			}
		})
	}
}

func TestNormalizeCatalogRejectsUnknownGrantPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		submit  string
		wantErr bool
	}{
		{submit: "TENANT_ADMIN_ONLY", wantErr: true},
		{submit: "super-admin-only", wantErr: true},
		{submit: "1", wantErr: true},
		// 尾随空白与大小写由归一处理，属于合法输入。
		{submit: "SUPER_ADMIN_ONLY ", wantErr: false},
		{submit: " default ", wantErr: false},
	}
	for _, testCase := range cases {
		_, _, err := normalizeCatalog(CatalogInput{
			CatalogVersion: "1",
			Permissions: []PermissionInput{{
				Code: "customer.read.all", Name: "查看全部客户", Action: "read", ResourceCode: "customer",
				GrantPolicy: testCase.submit,
			}},
		})
		if testCase.wantErr {
			if err == nil || !errorsIs(err, ErrValidation) {
				t.Fatalf("grant_policy %q must be rejected with a validation error, got %v", testCase.submit, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("grant_policy %q must be normalized and accepted: %v", testCase.submit, err)
		}
	}
}

func TestNormalizeCatalogIncludesGrantPolicyInChecksum(t *testing.T) {
	t.Parallel()

	base := CatalogInput{
		CatalogVersion: "2026.11.05",
		Permissions: []PermissionInput{{
			Code: "customer.read.all", Name: "查看全部客户", Action: "read", ResourceCode: "customer",
			GrantPolicy: authzdomain.GrantPolicyDefault,
		}},
		Roles: []CatalogRoleInput{{Code: "customer_all_viewer", Name: "全部客户查看者", Permissions: []string{"customer.read.all"}}},
	}
	_, defaultChecksum, err := normalizeCatalog(base)
	if err != nil {
		t.Fatalf("normalize DEFAULT catalog: %v", err)
	}

	restricted := base
	restricted.Permissions = []PermissionInput{{
		Code: "customer.read.all", Name: "查看全部客户", Action: "read", ResourceCode: "customer",
		GrantPolicy: authzdomain.GrantPolicySuperAdminOnly,
	}}
	normalizedRestricted, restrictedChecksum, err := normalizeCatalog(restricted)
	if err != nil {
		t.Fatalf("normalize SUPER_ADMIN_ONLY catalog: %v", err)
	}
	if defaultChecksum == restrictedChecksum {
		t.Fatal("catalog checksum must include grant_policy")
	}

	// 摘要按逐字节比对，因此 grant_policy 必须真的出现在 canonical 载荷里。
	encoded, err := json.Marshal(normalizedRestricted.Permissions)
	if err != nil {
		t.Fatalf("marshal normalized permissions: %v", err)
	}
	if !strings.Contains(string(encoded), `"grant_policy":"SUPER_ADMIN_ONLY"`) {
		t.Fatalf("canonical payload lost grant_policy: %s", encoded)
	}

	// 未声明（空串）不得被摘要归一成 DEFAULT：两者是不同目录内容，会被订阅方拒绝。
	unset := base
	unset.Permissions = []PermissionInput{{
		Code: "customer.read.all", Name: "查看全部客户", Action: "read", ResourceCode: "customer",
	}}
	normalizedUnset, unsetChecksum, err := normalizeCatalog(unset)
	if err != nil {
		t.Fatalf("normalize unset grant_policy catalog: %v", err)
	}
	if normalizedUnset.Permissions[0].GrantPolicy != "" {
		t.Fatalf("unset grant_policy must stay empty in canonical form, got %q", normalizedUnset.Permissions[0].GrantPolicy)
	}
	if unsetChecksum == defaultChecksum {
		t.Fatal("unset grant_policy must not be treated as explicit DEFAULT in the checksum")
	}
}

func TestCatalogGrantPolicyDefaultsToDefaultOnPersistence(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "  ", authzdomain.GrantPolicyDefault, "default"} {
		if got := catalogGrantPolicy(value); got != authzdomain.GrantPolicyDefault {
			t.Fatalf("catalogGrantPolicy(%q) = %q, want %q", value, got, authzdomain.GrantPolicyDefault)
		}
	}
	if got := catalogGrantPolicy(authzdomain.GrantPolicySuperAdminOnly); got != authzdomain.GrantPolicySuperAdminOnly {
		t.Fatalf("catalogGrantPolicy must preserve SUPER_ADMIN_ONLY, got %q", got)
	}
}

func TestCatalogRoleAssignableOnlyRestrictsSuperAdminOnlyRoles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		restricted bool
		superAdmin bool
		want       bool
	}{
		{name: "ordinary role for ordinary operator", restricted: false, superAdmin: false, want: true},
		{name: "ordinary role for super administrator", restricted: false, superAdmin: true, want: true},
		{name: "restricted role for ordinary operator", restricted: true, superAdmin: false, want: false},
		{name: "restricted role for super administrator", restricted: true, superAdmin: true, want: true},
	}
	for _, testCase := range cases {
		if got := catalogRoleAssignable(testCase.restricted, testCase.superAdmin); got != testCase.want {
			t.Fatalf("%s: catalogRoleAssignable(%v, %v) = %v, want %v", testCase.name, testCase.restricted, testCase.superAdmin, got, testCase.want)
		}
	}
}

type catalogGrantPolicyPermission struct {
	Code    string
	Policy  string
	Granted bool
}

type catalogGrantPolicyScenario struct {
	permissions []catalogGrantPolicyPermission
	queries     []string
}

type catalogGrantPolicyRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *catalogGrantPolicyRows) Columns() []string { return rows.columns }
func (*catalogGrantPolicyRows) Close() error           { return nil }

func (rows *catalogGrantPolicyRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type catalogGrantPolicyDriver struct{ scenario *catalogGrantPolicyScenario }
type catalogGrantPolicyConn struct{ scenario *catalogGrantPolicyScenario }
type catalogGrantPolicyTx struct{}

func (catalogGrantPolicyTx) Commit() error   { return nil }
func (catalogGrantPolicyTx) Rollback() error { return nil }

func (d *catalogGrantPolicyDriver) Open(string) (driver.Conn, error) {
	return &catalogGrantPolicyConn{scenario: d.scenario}, nil
}
func (*catalogGrantPolicyConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unexpected prepared SQL")
}
func (*catalogGrantPolicyConn) Close() error              { return nil }
func (*catalogGrantPolicyConn) Begin() (driver.Tx, error) { return catalogGrantPolicyTx{}, nil }
func (*catalogGrantPolicyConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return catalogGrantPolicyTx{}, nil
}
func (*catalogGrantPolicyConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return catalogBoundaryResult{}, nil
}

func (c *catalogGrantPolicyConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	switch {
	case strings.Contains(normalized, "from authz_application_authorization_policy"):
		return &catalogGrantPolicyRows{columns: []string{"tenant_id"}}, nil
	case strings.Contains(normalized, "from authz_permission as p"):
		values := make([][]driver.Value, 0, len(c.scenario.permissions))
		for index, permission := range c.scenario.permissions {
			values = append(values, []driver.Value{
				fmt.Sprintf("permission-%d", index), permission.Code, permission.Code, "read",
				"customer", "HIGH", permission.Policy, "ACTIVE",
			})
		}
		return &catalogGrantPolicyRows{
			columns: []string{"id", "code", "name", "action", "resource_code", "risk_level", "grant_policy", "status"},
			values:  values,
		}, nil
	case strings.Contains(normalized, "from authz_role where"):
		return &catalogGrantPolicyRows{
			columns: []string{"id", "code", "name", "description", "status"},
			values:  [][]driver.Value{{"role-customer-all-viewer", "customer_all_viewer", "全部客户查看者", "", "ACTIVE"}},
		}, nil
	case strings.Contains(normalized, "from authz_role_permission as rp"):
		values := make([][]driver.Value, 0, len(c.scenario.permissions))
		for _, permission := range c.scenario.permissions {
			if permission.Granted {
				values = append(values, []driver.Value{permission.Code, permission.Policy})
			}
		}
		return &catalogGrantPolicyRows{columns: []string{"code", "grant_policy"}, values: values}, nil
	default:
		return &catalogGrantPolicyRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	}
}

var catalogGrantPolicyDriverCounter uint64

func newCatalogGrantPolicyService(t *testing.T, scenario *catalogGrantPolicyScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("appaccess-catalog-grant-policy-%d", atomic.AddUint64(&catalogGrantPolicyDriverCounter, 1))
	sql.Register(name, &catalogGrantPolicyDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open scripted catalog database: %v", err)
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

func superAdminCatalogContext(tenantID, userID string) context.Context {
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: tenantID},
		User:   authctx.ReferenceName{ID: userID},
		Roles:  []authctx.ReferenceName{{Code: authzdomain.SuperAdminRoleCode}},
	})
}

func catalogGrantPolicyView(t *testing.T, ctx context.Context, scenario *catalogGrantPolicyScenario) CatalogView {
	t.Helper()
	service := newCatalogGrantPolicyService(t, scenario)
	view, err := service.catalogView(ctx, applicationRow{ID: "app-crm", TenantID: "tenant-1", Code: "customer_and_opportunity"}, catalogMetadataRow{
		TenantID: "tenant-1", ApplicationID: "app-crm", CatalogVersion: "2026.11.05", CatalogHash: "sha256:test", SyncStatus: "SYNCED",
	})
	if err != nil {
		t.Fatalf("catalogView returned error: %v", err)
	}
	return view
}

func restrictedCatalogScenario() *catalogGrantPolicyScenario {
	return &catalogGrantPolicyScenario{permissions: []catalogGrantPolicyPermission{
		{Code: "customer.read", Policy: authzdomain.GrantPolicyDefault, Granted: true},
		{Code: "customer.read.all", Policy: authzdomain.GrantPolicySuperAdminOnly, Granted: true},
	}}
}

func TestCatalogViewHidesRestrictedRoleFromNonSuperAdministrator(t *testing.T) {
	scenario := restrictedCatalogScenario()
	// 普通操作者（即使是持 role-binding:update 的控制台用户）看不到该角色可选。
	view := catalogGrantPolicyView(t, principalContextForCatalog("tenant-1", "operator-1"), scenario)

	if len(view.Permissions) != 2 {
		t.Fatalf("permission view count = %d, want 2", len(view.Permissions))
	}
	policies := map[string]string{}
	for _, permission := range view.Permissions {
		policies[permission.Code] = permission.GrantPolicy
	}
	if policies["customer.read.all"] != authzdomain.GrantPolicySuperAdminOnly || policies["customer.read"] != authzdomain.GrantPolicyDefault {
		t.Fatalf("permission view lost grant_policy: %#v", policies)
	}
	if len(view.Roles) != 1 {
		t.Fatalf("role view count = %d, want 1", len(view.Roles))
	}
	if view.Roles[0].Assignable {
		t.Fatalf("super-administrator-only role must not be assignable for a non-super administrator: %#v", view.Roles[0])
	}
	if len(view.Roles[0].Permissions) != 2 {
		t.Fatalf("role permission list = %#v", view.Roles[0].Permissions)
	}

	joined := strings.ToLower(strings.Join(scenario.queries, "\n"))
	if !strings.Contains(joined, "p.grant_policy") {
		t.Fatalf("catalog view must read grant_policy: %s", joined)
	}
	if !strings.Contains(joined, "p.code, p.grant_policy") {
		t.Fatalf("role permission query must read grant_policy per permission: %s", joined)
	}
}

func TestCatalogViewExposesRestrictedRoleToSuperAdministrator(t *testing.T) {
	scenario := restrictedCatalogScenario()
	view := catalogGrantPolicyView(t, superAdminCatalogContext("tenant-1", "operator-1"), scenario)

	if len(view.Roles) != 1 || !view.Roles[0].Assignable {
		t.Fatalf("super administrator must be able to assign the restricted role: %#v", view.Roles)
	}
}

func TestCatalogViewKeepsDefaultRolesAssignableForEveryone(t *testing.T) {
	scenario := &catalogGrantPolicyScenario{permissions: []catalogGrantPolicyPermission{
		{Code: "customer.read", Policy: authzdomain.GrantPolicyDefault, Granted: true},
	}}
	view := catalogGrantPolicyView(t, principalContextForCatalog("tenant-1", "operator-1"), scenario)

	if len(view.Roles) != 1 || !view.Roles[0].Assignable {
		t.Fatalf("roles without restricted permissions must stay assignable: %#v", view.Roles)
	}
}

func principalContextForCatalog(tenantID, userID string) context.Context {
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: tenantID},
		User:   authctx.ReferenceName{ID: userID},
		Roles:  []authctx.ReferenceName{{Code: "platform-role-admin"}},
	})
}
