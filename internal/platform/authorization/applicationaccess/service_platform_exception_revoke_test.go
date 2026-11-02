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
	"time"

	authapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件回归 BIZ-IAM-03：撤销平台应用的个人例外必须真正停用权威绑定。
// 平台应用（code=platform）的内置角色目录是 PLATFORM 类型，读取路径一直按该类型取
// 可编辑集合；撤销路径此前硬编码 APPLICATION，命中 0 行却返回成功。
// 同时锁定配套的控制面保护：受保护角色只有租户级超级管理员可撤，部署初始化的
// 超管绑定不可撤销——撤销路径变“真”之后不能变成绕过保护的捷径。

type platformRevokeExec struct {
	query string
	args  []driver.Value
}

type platformRevokeScenario struct {
	applicationCode string
	bootstrapUserID string
	bootstrapAdmin  bool
	roleCodes       []string
	// environments 是 Keycloak Client 映射返回的环境，用于断言撤销后 outbox 真的入队。
	environments []string
	execs        []platformRevokeExec
	guardQueries int
}

type platformRevokeDriver struct{ scenario *platformRevokeScenario }

func (d *platformRevokeDriver) Open(string) (driver.Conn, error) {
	return &platformRevokeConn{scenario: d.scenario}, nil
}

type platformRevokeConn struct{ scenario *platformRevokeScenario }

func (c *platformRevokeConn) Prepare(query string) (driver.Stmt, error) {
	return &platformRevokeStmt{conn: c, query: query}, nil
}
func (*platformRevokeConn) Close() error { return nil }
func (*platformRevokeConn) Begin() (driver.Tx, error) {
	return platformRevokeTx{}, nil
}

type platformRevokeTx struct{}

func (platformRevokeTx) Commit() error   { return nil }
func (platformRevokeTx) Rollback() error { return nil }

type platformRevokeStmt struct {
	conn  *platformRevokeConn
	query string
}

func (*platformRevokeStmt) Close() error  { return nil }
func (*platformRevokeStmt) NumInput() int { return -1 }

func (s *platformRevokeStmt) Exec(args []driver.Value) (driver.Result, error) {
	q := strings.ToLower(strings.ReplaceAll(s.query, "`", ""))
	switch {
	case strings.HasPrefix(q, "update authz_role_binding"),
		strings.HasPrefix(q, "insert into authz_role_binding"),
		strings.HasPrefix(q, "delete from authz_user_permission"),
		strings.HasPrefix(q, "insert into authz_policy_revision"),
		strings.HasPrefix(q, "insert into keycloak_authorization_outbox"):
		s.conn.scenario.execs = append(s.conn.scenario.execs, platformRevokeExec{query: s.query, args: append([]driver.Value(nil), args...)})
		return platformRevokeResult{}, nil
	}
	return nil, fmt.Errorf("unexpected write: %s", s.query)
}

type platformRevokeResult struct{}

func (platformRevokeResult) LastInsertId() (int64, error) { return 1, nil }
func (platformRevokeResult) RowsAffected() (int64, error) { return 1, nil }

func (s *platformRevokeStmt) Query([]driver.Value) (driver.Rows, error) {
	lower := strings.ToLower(strings.ReplaceAll(s.query, "`", ""))
	scenario := s.conn.scenario
	switch {
	case strings.Contains(lower, "authz_role_binding") && strings.Contains(lower, "distinct"):
		// 撤销前的控制面保护查询：返回目标主体当前的 MANUAL 平台角色码。
		scenario.guardQueries++
		rows := &platformRevokeRows{columns: []string{"code"}}
		for _, code := range scenario.roleCodes {
			rows.values = append(rows.values, []driver.Value{code})
		}
		return rows, nil
	case strings.Contains(lower, "from iam_bootstrap_state"):
		rows := &platformRevokeRows{columns: []string{"tenant_id", "first_super_admin_user_id", "first_super_admin_account_id"}}
		if scenario.bootstrapAdmin {
			rows.values = append(rows.values, []driver.Value{"tenant-1", scenario.bootstrapUserID, "account-1"})
		}
		return rows, nil
	case strings.Contains(lower, "from iam_tenant"):
		if strings.Contains(lower, "code") && !strings.Contains(lower, "first_super_admin") {
			return &platformRevokeRows{columns: []string{"code"}, values: [][]driver.Value{{"tenant-1"}}}, nil
		}
		return &platformRevokeRows{columns: []string{"id"}, values: [][]driver.Value{{"tenant-1"}}}, nil
	case strings.Contains(lower, "from platform_application"):
		return &platformRevokeRows{columns: []string{"id", "tenant_id", "code"}, values: [][]driver.Value{{"app-1", "tenant-1", scenario.applicationCode}}}, nil
	case strings.Contains(lower, "from iam_user"):
		return &platformRevokeRows{columns: []string{"status", "deleted_at"}, values: [][]driver.Value{{activeStatus, nil}}}, nil
	case strings.Contains(lower, "from iam_account"):
		return &platformRevokeRows{columns: []string{"user_id", "status"}, values: [][]driver.Value{{scenario.bootstrapUserID, activeStatus}}}, nil
	case strings.Contains(lower, "count(") && (strings.Contains(lower, "authz_role_binding") || strings.Contains(lower, "iam_org_unit") || strings.Contains(lower, "iam_position")):
		return &platformRevokeRows{columns: []string{"count"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(lower, "from authz_application_authorization_policy"):
		return &platformRevokeRows{columns: []string{"application_id"}, values: [][]driver.Value{{"app-1"}}}, nil
	case strings.Contains(lower, "from authz_authorization_catalog"):
		return &platformRevokeRows{columns: []string{"claims_role_config_hash"}, values: [][]driver.Value{{"catalog-hash"}}}, nil
	case strings.Contains(lower, "from authz_policy_revision"):
		return &platformRevokeRows{columns: []string{"revision"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(lower, "from keycloak_application_client_mapping"):
		rows := &platformRevokeRows{columns: []string{"environment_id"}}
		for _, environmentID := range scenario.environments {
			rows.values = append(rows.values, []driver.Value{environmentID})
		}
		return rows, nil
	default:
		// 有效角色、MANUAL 管理视图、edit revision 与历史自定义权限读：空集是合法业务状态。
		return &platformRevokeRows{columns: []string{"value"}}, nil
	}
}

type platformRevokeRows struct {
	columns []string
	values  [][]driver.Value
	cursor  int
}

func (r *platformRevokeRows) Columns() []string { return r.columns }
func (*platformRevokeRows) Close() error        { return nil }
func (r *platformRevokeRows) Next(dest []driver.Value) error {
	if r.cursor >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.cursor])
	r.cursor++
	return nil
}

var platformRevokeDriverSeq uint64

func newPlatformRevokeService(t *testing.T, scenario *platformRevokeScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("appaccess-platform-revoke-%d", atomic.AddUint64(&platformRevokeDriverSeq, 1))
	sql.Register(name, &platformRevokeDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{db: db, ids: writeBoundaryIDs{}, clock: writeBoundaryClock{time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}}
}

func platformRevokeDeleteInput() DeleteAccessInput {
	return DeleteAccessInput{TenantID: "tenant-1", UserID: "user-1", OperatorID: "operator-1"}
}

func platformRevokeArg(args []driver.Value, want string) bool {
	for _, arg := range args {
		if value, ok := arg.(string); ok && value == want {
			return true
		}
	}
	return false
}

func platformRevokeBindingUpdates(scenario *platformRevokeScenario) []platformRevokeExec {
	var updates []platformRevokeExec
	for _, exec := range scenario.execs {
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(exec.query, "`", "")), "update authz_role_binding") {
			updates = append(updates, exec)
		}
	}
	return updates
}

// TestDeleteAccessRevokesPlatformPersonalException 是本缺陷的直接回归：平台应用的
// 个人例外必须按 PLATFORM 角色类型停用，撤销不再命中 0 行。
func TestDeleteAccessRevokesPlatformPersonalException(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: PlatformApplicationCode, roleCodes: []string{"platform-audit-admin"}}
	service := newPlatformRevokeService(t, scenario)

	if err := service.DeleteAccess(context.Background(), platformRevokeDeleteInput(), PlatformApplicationCode); err != nil {
		t.Fatalf("revoking a platform personal exception must succeed: %v", err)
	}
	if scenario.guardQueries != 1 {
		t.Fatalf("revocation must run the control-plane guard exactly once, got %d", scenario.guardQueries)
	}
	updates := platformRevokeBindingUpdates(scenario)
	if len(updates) != 1 {
		t.Fatalf("platform exception revocation must issue exactly one binding update, got %d: %v", len(updates), scenario.execs)
	}
	if !platformRevokeArg(updates[0].args, platformRoleType) {
		t.Fatalf("platform exception revocation must target %q role bindings, args=%v sql=%s", platformRoleType, updates[0].args, updates[0].query)
	}
	if platformRevokeArg(updates[0].args, applicationRoleType) {
		t.Fatalf("platform exception revocation must not fall back to %q roles: args=%v", applicationRoleType, updates[0].args)
	}
	for _, expected := range []string{"tenant-1", "app-1", "user-1", "operator-1", grantOriginManual, disabledStatus} {
		if !platformRevokeArg(updates[0].args, expected) {
			t.Fatalf("revocation lost boundary %q: args=%v", expected, updates[0].args)
		}
	}
}

// platformRevokeAuditRecorder 捕获撤销入口产生的应用授权审计事件，用于断言 changed 语义。
type platformRevokeAuditRecorder struct{ events []AuditEvent }

func (r *platformRevokeAuditRecorder) RecordApplicationAccessAudit(_ context.Context, event AuditEvent) error {
	r.events = append(r.events, event)
	return nil
}

// platformRevokeExecsWithPrefix 返回与该写语句前缀匹配的执行记录。
func platformRevokeExecsWithPrefix(scenario *platformRevokeScenario, prefix string) []platformRevokeExec {
	matched := make([]platformRevokeExec, 0, 1)
	for _, exec := range scenario.execs {
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(exec.query, "`", "")), prefix) {
			matched = append(matched, exec)
		}
	}
	return matched
}

// TestDeleteAccessPlatformExceptionBumpsRevisionAndEnqueuesOutbox 锁定“撤销变真”后的
// 下游事实：命中绑定后 changed:true ⇒ 授权 revision 递增、Keycloak outbox 入队，
// 审计 metadata 如实记录 changed=true（BIZ-IAM-03 语义面）。
func TestDeleteAccessPlatformExceptionBumpsRevisionAndEnqueuesOutbox(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: PlatformApplicationCode, roleCodes: []string{"platform-audit-admin"}, environments: []string{"env-prod"}}
	service := newPlatformRevokeService(t, scenario)
	audit := &platformRevokeAuditRecorder{}
	service.audit = audit

	if err := service.DeleteAccess(context.Background(), platformRevokeDeleteInput(), PlatformApplicationCode); err != nil {
		t.Fatalf("revoking a platform personal exception must succeed: %v", err)
	}
	updates := platformRevokeBindingUpdates(scenario)
	if len(updates) != 1 {
		t.Fatalf("platform exception revocation must issue exactly one binding update, got %d: %v", len(updates), scenario.execs)
	}
	for _, expected := range []string{disabledStatus, platformRoleType, grantOriginManual, activeStatus} {
		if !platformRevokeArg(updates[0].args, expected) {
			t.Fatalf("binding update lost %q: args=%v sql=%s", expected, updates[0].args, updates[0].query)
		}
	}
	revisions := platformRevokeExecsWithPrefix(scenario, "insert into authz_policy_revision")
	if len(revisions) != 1 {
		t.Fatalf("changed revocation must bump the authorization revision exactly once: %v", scenario.execs)
	}
	for _, expected := range []string{"tenant-1", "app-1"} {
		if !platformRevokeArg(revisions[0].args, expected) {
			t.Fatalf("revision bump lost boundary %q: args=%v", expected, revisions[0].args)
		}
	}
	outbox := platformRevokeExecsWithPrefix(scenario, "insert into keycloak_authorization_outbox")
	if len(outbox) != 1 {
		t.Fatalf("changed revocation must enqueue exactly one Keycloak outbox row: %v", scenario.execs)
	}
	for _, expected := range []string{"env-prod", "user-1", "app-1", "AUTHORIZATION_CHANGED"} {
		if !platformRevokeArg(outbox[0].args, expected) {
			t.Fatalf("outbox row lost %q: args=%v", expected, outbox[0].args)
		}
	}
	if len(audit.events) != 1 {
		t.Fatalf("revocation must record exactly one audit event, got %d: %+v", len(audit.events), audit.events)
	}
	event := audit.events[0]
	if event.Action != "authorization.application_access.deleted" || event.Result != "SUCCESS" {
		t.Fatalf("audit event = %s/%s, want authorization.application_access.deleted/SUCCESS", event.Action, event.Result)
	}
	if changed, ok := event.Metadata["changed"]; !ok || changed != true {
		t.Fatalf("audit metadata changed = %v (present=%v), want true", changed, ok)
	}
	if origin, ok := event.Metadata["grant_origin"]; !ok || origin != grantOriginManual {
		t.Fatalf("audit metadata grant_origin = %v (present=%v), want %q", origin, ok, grantOriginManual)
	}
}

// TestDeleteAccessKeepsApplicationRoleScopeForOrdinaryApplications 固定普通应用行为：
// 仍然只撤销 APPLICATION 类型的人工绑定，避免把平台例外修复扩大到全部应用。
func TestDeleteAccessKeepsApplicationRoleScopeForOrdinaryApplications(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: "sales-app"}
	service := newPlatformRevokeService(t, scenario)

	if err := service.DeleteAccess(context.Background(), platformRevokeDeleteInput(), "sales-app"); err != nil {
		t.Fatalf("ordinary application revocation must succeed: %v", err)
	}
	if scenario.guardQueries != 0 {
		t.Fatalf("ordinary applications must not run the platform control-plane guard, got %d queries", scenario.guardQueries)
	}
	updates := platformRevokeBindingUpdates(scenario)
	if len(updates) != 1 || !platformRevokeArg(updates[0].args, applicationRoleType) || platformRevokeArg(updates[0].args, platformRoleType) {
		t.Fatalf("ordinary application revocation must keep %q scope: %v", applicationRoleType, updates)
	}
}

// TestDeleteAccessRejectsProtectedPlatformRoleWithoutSuperAdminOperator 防止撤销入口
// 变成绕过控制面保护的捷径：非租户级超级管理员不得撤销受保护平台角色。
func TestDeleteAccessRejectsProtectedPlatformRoleWithoutSuperAdminOperator(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: PlatformApplicationCode, roleCodes: []string{"platform-security-admin", BootstrapSuperAdminRoleCode}}
	service := newPlatformRevokeService(t, scenario)

	err := service.DeleteAccess(context.Background(), platformRevokeDeleteInput(), PlatformApplicationCode)
	if !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("protected platform role revocation must fail closed with ErrForbidden, got %v", err)
	}
	if len(scenario.execs) != 0 {
		t.Fatalf("rejected revocation must not write anything: %v", scenario.execs)
	}
}

// TestDeleteAccessAllowsProtectedPlatformRoleForSuperAdminOperator 是上一用例的对照：
// 已认证的租户级超级管理员仍可撤销受保护角色的个人例外。
func TestDeleteAccessAllowsProtectedPlatformRoleForSuperAdminOperator(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: PlatformApplicationCode, roleCodes: []string{"platform-security-admin"}}
	service := newPlatformRevokeService(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "operator-1"},
		Roles:  []authctx.ReferenceName{{Code: "platform-super-admin"}},
	})

	if err := service.DeleteAccess(ctx, platformRevokeDeleteInput(), PlatformApplicationCode); err != nil {
		t.Fatalf("super administrator must be able to revoke protected platform exceptions: %v", err)
	}
	if len(platformRevokeBindingUpdates(scenario)) != 1 {
		t.Fatalf("super administrator revocation must still update the binding: %v", scenario.execs)
	}
}

// TestDeleteAccessRefusesBootstrapSuperAdminBinding 保证撤销变“真”之后不会削弱
// 部署初始化管理员：该绑定在任何入口都不可撤销。
func TestDeleteAccessRefusesBootstrapSuperAdminBinding(t *testing.T) {
	scenario := &platformRevokeScenario{
		applicationCode: PlatformApplicationCode,
		bootstrapAdmin:  true,
		bootstrapUserID: "user-1",
		roleCodes:       []string{BootstrapSuperAdminRoleCode},
	}
	service := newPlatformRevokeService(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "operator-1"},
		Roles:  []authctx.ReferenceName{{Code: "platform-super-admin"}},
	})

	err := service.DeleteAccess(ctx, platformRevokeDeleteInput(), PlatformApplicationCode)
	if !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("bootstrap super administrator binding must be protected, got %v", err)
	}
	if len(scenario.execs) != 0 {
		t.Fatalf("protected bootstrap revocation must not write anything: %v", scenario.execs)
	}
}

// TestDeleteSubjectAccessRevokesPlatformSubjectException 保证组织/岗位绑定与用户路径
// 使用同一角色类型口径：读取与撤销必须看到同一集合，撤销不再返回“成功但未变更”。
func TestDeleteSubjectAccessRevokesPlatformSubjectException(t *testing.T) {
	scenario := &platformRevokeScenario{applicationCode: PlatformApplicationCode, roleCodes: []string{"platform-audit-admin"}}
	service := newPlatformRevokeService(t, scenario)

	err := service.DeleteSubjectAccess(context.Background(), DeleteSubjectAccessInput{
		TenantID: "tenant-1", SubjectType: subjectTypeOrgUnit, SubjectID: "org-unit-1", OperatorID: "operator-1",
	}, PlatformApplicationCode)
	if err != nil {
		t.Fatalf("revoking a platform subject exception must succeed: %v", err)
	}
	updates := platformRevokeBindingUpdates(scenario)
	if len(updates) != 1 || !platformRevokeArg(updates[0].args, platformRoleType) || platformRevokeArg(updates[0].args, applicationRoleType) {
		t.Fatalf("platform subject revocation must target %q roles: %v", platformRoleType, updates)
	}
	if !platformRevokeArg(updates[0].args, "org-unit-1") {
		t.Fatalf("subject revocation lost subject boundary: %v", updates[0].args)
	}
}

// TestManagedAccessRoleTypeMatchesReadPath 固定平台应用与普通应用的角色类型口径。
func TestManagedAccessRoleTypeMatchesReadPath(t *testing.T) {
	if got := managedAccessRoleType(PlatformApplicationCode); got != platformRoleType {
		t.Fatalf("platform application role type = %q, want %q", got, platformRoleType)
	}
	if got := managedAccessRoleType("sales-app"); got != applicationRoleType {
		t.Fatalf("ordinary application role type = %q, want %q", got, applicationRoleType)
	}
	if got := managedAccessRoleType(" platform "); got != platformRoleType {
		t.Fatalf("padded platform code must normalize, got %q", got)
	}
}
