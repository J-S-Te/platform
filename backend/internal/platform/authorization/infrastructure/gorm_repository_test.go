package infrastructure

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
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
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

func TestCreateRoleBindingRejectsRoleOutsideOperatorDelegablePermissions(t *testing.T) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-high": {ID: "role-high", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-high", Name: "High privilege custom role", RoleType: "CUSTOM", Status: domain.StatusActive},
		},
		rolePermissions: map[string][]string{"role-high": {"permission-high"}},
		delegableCount:  0,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "security-admin"},
		Account: authctx.ReferenceName{ID: "security-account"},
	})

	_, err := repository.CreateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-new", Role: domain.Reference{ID: "role-high"}, SubjectType: "USER", Subject: domain.Reference{ID: "security-admin"}, ScopeType: "TENANT",
	})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("CreateRoleBinding high privilege role error = %v, want forbidden", err)
	}
	if scenario.execCount != 0 {
		t.Fatalf("CreateRoleBinding executed %d writes after delegation denial", scenario.execCount)
	}
}

func TestCreateRoleBindingAllowsRoleWithinOperatorDelegablePermissions(t *testing.T) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-readable": {ID: "role-readable", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-readable", Name: "Readable role", RoleType: "CUSTOM", Status: domain.StatusActive},
		},
		rolePermissions: map[string][]string{"role-readable": {"permission-read"}},
		delegableCount:  1,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "security-admin"},
		Account: authctx.ReferenceName{ID: "security-account"},
	})

	created, err := repository.CreateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-new", Role: domain.Reference{ID: "role-readable"}, SubjectType: "USER", Subject: domain.Reference{ID: "user-2"}, ScopeType: "TENANT",
	})
	if err != nil {
		t.Fatalf("CreateRoleBinding delegable role error = %v", err)
	}
	if created.Role.ID != "role-readable" || created.Subject.ID != "user-2" {
		t.Fatalf("unexpected created binding: %+v", created)
	}
	if scenario.execCount < 2 {
		t.Fatalf("CreateRoleBinding writes = %d, want binding insert and revision update", scenario.execCount)
	}
}

func TestUpdateRoleBindingRejectsReplacementWithRoleOutsideOperatorDelegablePermissions(t *testing.T) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-low":  {ID: "role-low", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-low", Name: "Low role", RoleType: "CUSTOM", Status: domain.StatusActive},
			"role-high": {ID: "role-high", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-high", Name: "High role", RoleType: "CUSTOM", Status: domain.StatusActive},
		},
		existingBinding: &roleBindingModel{ID: "binding-1", TenantID: "tenant-1", ApplicationID: "app-platform", RoleID: "role-low", SubjectType: "USER", SubjectID: "user-2", ScopeType: "TENANT", Status: domain.StatusActive, Version: 3},
		rolePermissions: map[string][]string{"role-high": {"permission-high"}},
		delegableCount:  0,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "security-admin"},
		Account: authctx.ReferenceName{ID: "security-account"},
	})

	_, err := repository.UpdateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-1", Role: domain.Reference{ID: "role-high"}, SubjectType: "USER", Subject: domain.Reference{ID: "security-admin"}, ScopeType: "TENANT", Status: domain.StatusActive, Version: 3,
	})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("UpdateRoleBinding high privilege replacement error = %v, want forbidden", err)
	}
	if scenario.execCount != 0 {
		t.Fatalf("UpdateRoleBinding executed %d writes after delegation denial", scenario.execCount)
	}
}

func TestUpdateRoleBindingRejectsDisablingRoleOutsideOperatorDelegablePermissions(t *testing.T) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-high": {ID: "role-high", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-high", Name: "High role", RoleType: "CUSTOM", Status: domain.StatusActive},
		},
		existingBinding: &roleBindingModel{ID: "binding-1", TenantID: "tenant-1", ApplicationID: "app-platform", RoleID: "role-high", SubjectType: "USER", SubjectID: "user-2", ScopeType: "TENANT", Status: domain.StatusActive, Version: 3},
		rolePermissions: map[string][]string{"role-high": {"permission-high"}},
		delegableCount:  0,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "security-admin"},
		Account: authctx.ReferenceName{ID: "security-account"},
	})

	_, err := repository.UpdateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-1", Role: domain.Reference{ID: "role-high"}, SubjectType: "USER", Subject: domain.Reference{ID: "user-2"}, ScopeType: "TENANT", Status: domain.StatusDisabled, Version: 3,
	})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("UpdateRoleBinding unauthorized disable error = %v, want forbidden", err)
	}
	if scenario.execCount != 0 {
		t.Fatalf("UpdateRoleBinding executed %d writes after disable delegation denial", scenario.execCount)
	}
}

func TestCreateRoleBindingKeepsProtectedRoleGuard(t *testing.T) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-super-admin": {ID: "role-super-admin", TenantID: "tenant-1", ApplicationID: "app-platform", Code: platformSuperAdminRoleCode, Name: "Super administrator", RoleType: "PLATFORM", BuiltIn: true, Status: domain.StatusActive},
		},
		protectedAdminCount: 0,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "security-admin"},
		Account: authctx.ReferenceName{ID: "security-account"},
	})

	_, err := repository.CreateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-super-admin", Role: domain.Reference{ID: "role-super-admin"}, SubjectType: "USER", Subject: domain.Reference{ID: "security-admin"}, ScopeType: "TENANT",
	})
	if !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("CreateRoleBinding protected role error = %v, want forbidden", err)
	}
	if scenario.execCount != 0 {
		t.Fatalf("CreateRoleBinding executed %d writes after protected-role denial", scenario.execCount)
	}
}

func TestCreateRoleBindingRejectsCrossTenantOrMissingScopeReference(t *testing.T) {
	missing := int64(0)
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-readable": {ID: "role-readable", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-readable", Name: "Readable role", RoleType: "CUSTOM", Status: domain.StatusActive},
		},
		rolePermissions: map[string][]string{"role-readable": {"permission-read"}},
		delegableCount:  1,
		referenceCount:  &missing,
	}
	repository := newRoleBindingTestRepository(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "security-admin"}, Account: authctx.ReferenceName{ID: "security-account"},
	})
	scopeID := "org-from-another-tenant"
	_, err := repository.CreateRoleBinding(ctx, "tenant-1", "security-admin", domain.RoleBinding{
		ID: "binding-new", Role: domain.Reference{ID: "role-readable"}, SubjectType: "USER", Subject: domain.Reference{ID: "user-2"}, ScopeType: "ORG_UNIT", ScopeID: &scopeID,
	})
	if !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("CreateRoleBinding missing tenant reference error = %v, want not found", err)
	}
	if scenario.execCount != 0 {
		t.Fatalf("CreateRoleBinding executed %d writes after reference denial", scenario.execCount)
	}
}

func TestOperatorAccountIDFromContextUsesOnlyMatchingAuthenticatedPrincipal(t *testing.T) {
	principal := authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "operator-1"},
		Account: authctx.ReferenceName{ID: "account-1"},
	}
	ctx := authctx.WithPrincipal(context.Background(), principal)
	if got := operatorAccountIDFromContext(ctx, "tenant-1", "operator-1"); got != "account-1" {
		t.Fatalf("matching principal account = %q, want account-1", got)
	}
	if got := operatorAccountIDFromContext(ctx, "tenant-2", "operator-1"); got != "" {
		t.Fatalf("cross-tenant principal account = %q, want empty", got)
	}
	if got := operatorAccountIDFromContext(ctx, "tenant-1", "operator-2"); got != "" {
		t.Fatalf("different operator principal account = %q, want empty", got)
	}
}

type roleBindingRepositoryScenario struct {
	roles               map[string]roleModel
	existingBinding     *roleBindingModel
	rolePermissions     map[string][]string
	delegableCount      int64
	protectedAdminCount int64
	referenceCount      *int64
	execCount           int
}

type roleBindingTestDriver struct {
	scenario *roleBindingRepositoryScenario
}

type roleBindingTestConn struct {
	scenario *roleBindingRepositoryScenario
}

type roleBindingTestTx struct{}

type roleBindingTestResult int64

type roleBindingTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

var roleBindingTestDriverCounter uint64

func newRoleBindingTestRepository(t *testing.T, scenario *roleBindingRepositoryScenario) *GORMRepository {
	t.Helper()
	driverName := fmt.Sprintf("authorization-role-binding-test-%d", atomic.AddUint64(&roleBindingTestDriverCounter, 1))
	sql.Register(driverName, &roleBindingTestDriver{scenario: scenario})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open role binding test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open role binding test gorm database: %v", err)
	}
	repository, err := NewGORMRepository(database)
	if err != nil {
		t.Fatalf("new role binding repository: %v", err)
	}
	return repository
}

func (d *roleBindingTestDriver) Open(string) (driver.Conn, error) {
	return &roleBindingTestConn{scenario: d.scenario}, nil
}

func (c *roleBindingTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by role binding test driver")
}
func (c *roleBindingTestConn) Close() error              { return nil }
func (c *roleBindingTestConn) Begin() (driver.Tx, error) { return roleBindingTestTx{}, nil }
func (c *roleBindingTestConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return roleBindingTestTx{}, nil
}
func (roleBindingTestTx) Commit() error                      { return nil }
func (roleBindingTestTx) Rollback() error                    { return nil }
func (r roleBindingTestResult) LastInsertId() (int64, error) { return int64(r), nil }
func (r roleBindingTestResult) RowsAffected() (int64, error) { return int64(r), nil }

func (c *roleBindingTestConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.execCount++
	return roleBindingTestResult(1), nil
}

func (c *roleBindingTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "operator_role.code"):
		return singleInt64Rows("count", c.scenario.protectedAdminCount), nil
	case strings.Contains(normalized, "count(distinct") && strings.Contains(normalized, "authz_role_permission"):
		return singleInt64Rows("count", c.scenario.delegableCount), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from `iam_user`") || strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from iam_user"):
		return singleInt64Rows("count", roleBindingReferenceCount(c.scenario)), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from `iam_account`") || strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from iam_account"):
		return singleInt64Rows("count", roleBindingReferenceCount(c.scenario)), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from `iam_org_unit`") || strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from iam_org_unit"):
		return singleInt64Rows("count", roleBindingReferenceCount(c.scenario)), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from `iam_position`") || strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from iam_position"):
		return singleInt64Rows("count", roleBindingReferenceCount(c.scenario)), nil
	case strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from `iam_membership`") || strings.HasPrefix(strings.TrimSpace(normalized), "select count(*) from iam_membership"):
		return singleInt64Rows("count", roleBindingReferenceCount(c.scenario)), nil
	case strings.Contains(normalized, "from authz_role_permission") || strings.Contains(normalized, "from `authz_role_permission`"):
		roleID := namedString(args, 3)
		if roleID == "" {
			roleID = firstKnownRolePermission(c.scenario.rolePermissions)
		}
		permissions := c.scenario.rolePermissions[roleID]
		values := make([][]driver.Value, 0, len(permissions))
		for _, permissionID := range permissions {
			values = append(values, []driver.Value{permissionID})
		}
		return &roleBindingTestRows{columns: []string{"permission_id"}, values: values}, nil
	case strings.Contains(normalized, "from `authz_role_bindings`") || strings.Contains(normalized, "from `authz_role_binding`"):
		if c.scenario.existingBinding == nil {
			return emptyRows(roleBindingColumns()), nil
		}
		return roleBindingRows(*c.scenario.existingBinding), nil
	case strings.Contains(normalized, "from `authz_roles`") || strings.Contains(normalized, "from `authz_role`"):
		roleID := namedString(args, 0)
		role, ok := c.scenario.roles[roleID]
		if !ok {
			return emptyRows(roleColumns()), nil
		}
		return roleRows(role), nil
	default:
		return nil, fmt.Errorf("unexpected role binding test query: %s", query)
	}
}

func roleBindingReferenceCount(scenario *roleBindingRepositoryScenario) int64 {
	if scenario.referenceCount != nil {
		return *scenario.referenceCount
	}
	return 1
}

func namedString(args []driver.NamedValue, index int) string {
	if index < 0 || index >= len(args) {
		return ""
	}
	value, _ := args[index].Value.(string)
	return value
}

func firstKnownRolePermission(values map[string][]string) string {
	for roleID := range values {
		return roleID
	}
	return ""
}

func singleInt64Rows(column string, value int64) driver.Rows {
	return &roleBindingTestRows{columns: []string{column}, values: [][]driver.Value{{value}}}
}

func emptyRows(columns []string) driver.Rows {
	return &roleBindingTestRows{columns: columns}
}

func roleColumns() []string {
	return []string{"id", "tenant_id", "application_id", "code", "name", "role_type", "description", "built_in", "status", "version", "created_at", "created_by", "updated_at", "updated_by"}
}

func roleRows(role roleModel) driver.Rows {
	return &roleBindingTestRows{columns: roleColumns(), values: [][]driver.Value{{
		role.ID, role.TenantID, role.ApplicationID, role.Code, role.Name, role.RoleType, nullableString(role.Description), role.BuiltIn, role.Status, int64(role.Version), role.CreatedAt, nullableString(role.CreatedBy), role.UpdatedAt, nullableString(role.UpdatedBy),
	}}}
}

func roleBindingColumns() []string {
	return []string{"id", "tenant_id", "application_id", "role_id", "subject_type", "subject_id", "scope_type", "scope_id", "valid_until", "status", "version", "created_at", "created_by", "updated_at", "updated_by"}
}

func roleBindingRows(binding roleBindingModel) driver.Rows {
	return &roleBindingTestRows{columns: roleBindingColumns(), values: [][]driver.Value{{
		binding.ID, binding.TenantID, binding.ApplicationID, binding.RoleID, binding.SubjectType, binding.SubjectID, binding.ScopeType, binding.ScopeID, nullableTime(binding.ValidUntil), binding.Status, int64(binding.Version), binding.CreatedAt, nullableString(binding.CreatedBy), binding.UpdatedAt, nullableString(binding.UpdatedBy),
	}}}
}

func nullableString(value *string) driver.Value {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) driver.Value {
	if value == nil {
		return nil
	}
	return *value
}

func (r *roleBindingTestRows) Columns() []string { return r.columns }
func (r *roleBindingTestRows) Close() error      { return nil }
func (r *roleBindingTestRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}
