package infrastructure

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

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 移除持久化来源保护后，这些真实仓储调用就会写入
// 由来源管理的绑定，即使认证操作人是超级管理员也一样。
func TestUpdateRoleBindingRejectsNonManualOriginBeforeWriting(t *testing.T) {
	for _, origin := range []string{"TEMPLATE", "SYSTEM", "INHERITED", "UNKNOWN", " template ", " system "} {
		for _, superAdmin := range []bool{false, true} {
			for _, change := range []string{"replace_role", "move_subject", "disable", "change_scope", "change_expiry"} {
				t.Run(fmt.Sprintf("%s/super=%t/%s", origin, superAdmin, change), func(t *testing.T) {
					scenario, binding := originBindingFixture(origin)
					mutateOriginBinding(&binding, change)
					repository := newOriginBindingRepository(t, scenario, false)
					_, err := repository.UpdateRoleBinding(originOperatorContext(superAdmin), "tenant-1", "operator-1", binding)
					if !errors.Is(err, application.ErrConflict) {
						t.Fatalf("UpdateRoleBinding source=%q error=%v, want conflict", origin, err)
					}
					if scenario.execCount != 0 {
						t.Fatalf("source-owned binding caused %d writes", scenario.execCount)
					}
				})
			}
		}
	}
}

func TestUpdateRoleBindingAllowsManualOriginChanges(t *testing.T) {
	for _, origin := range []string{"MANUAL", "", " manual ", "  "} {
		for _, change := range []string{"replace_role", "move_subject", "disable", "change_scope", "change_expiry"} {
			t.Run(origin+"/"+change, func(t *testing.T) {
				scenario, binding := originBindingFixture(origin)
				mutateOriginBinding(&binding, change)
				repository := newOriginBindingRepository(t, scenario, false)
				got, err := repository.UpdateRoleBinding(originOperatorContext(false), "tenant-1", "operator-1", binding)
				if err != nil {
					t.Fatalf("UpdateRoleBinding manual change error=%v", err)
				}
				if got.Role.ID != binding.Role.ID || got.SubjectType != binding.SubjectType || got.Subject.ID != binding.Subject.ID || got.ScopeType != binding.ScopeType || got.Status != binding.Status || got.Version != 4 || got.GrantOrigin != origin {
					t.Fatalf("unexpected updated binding: %+v", got)
				}
				if (got.ScopeID == nil) != (binding.ScopeID == nil) || got.ScopeID != nil && *got.ScopeID != *binding.ScopeID {
					t.Fatalf("scope ID=%v, want %v", got.ScopeID, binding.ScopeID)
				}
				if (got.ExpiresAt == nil) != (binding.ExpiresAt == nil) || got.ExpiresAt != nil && !got.ExpiresAt.Equal(*binding.ExpiresAt) {
					t.Fatalf("expiry=%v, want %v", got.ExpiresAt, binding.ExpiresAt)
				}
				if scenario.execCount != 2 {
					t.Fatalf("writes=%d, want binding update and revision update", scenario.execCount)
				}
			})
		}
	}
}

func TestUpdateRoleBindingManualOriginRetainsOwnershipAndProtectionGuards(t *testing.T) {
	for _, guard := range []string{"application_target", "application_existing", "protected_target", "protected_existing", "bootstrap", "stale_version"} {
		t.Run(guard, func(t *testing.T) {
			scenario, binding := originBindingFixture("MANUAL")
			want := application.ErrConflict
			superAdmin := false
			switch guard {
			case "application_target":
				role := scenario.roles["role-original"]
				role.RoleType = "APPLICATION"
				scenario.roles[role.ID] = role
			case "application_existing":
				binding.Role.ID = "role-other"
				role := scenario.roles["role-original"]
				role.RoleType = "APPLICATION"
				scenario.roles[role.ID] = role
			case "protected_target":
				role := scenario.roles["role-original"]
				role.Code = domain.SuperAdminRoleCode
				scenario.roles[role.ID] = role
				want = application.ErrForbidden
			case "protected_existing":
				binding.Role.ID = "role-other"
				role := scenario.roles["role-original"]
				role.Code = domain.SuperAdminRoleCode
				scenario.roles[role.ID] = role
				want = application.ErrForbidden
			case "bootstrap":
				role := scenario.roles["role-original"]
				role.Code, role.BuiltIn = domain.SuperAdminRoleCode, true
				scenario.roles[role.ID] = role
				scenario.existingBinding.SubjectType, scenario.existingBinding.SubjectID = "USER", "bootstrap-user"
				binding.SubjectType, binding.Subject.ID, binding.Status = "USER", "bootstrap-user", domain.StatusDisabled
				scenario.protectedAdminCount, superAdmin = 1, true
				want = bootstrapadmin.ErrProtected
			case "stale_version":
				binding.Version = 2
				want = application.ErrVersionConflict
			}
			repository := newOriginBindingRepository(t, scenario, guard == "bootstrap")
			_, err := repository.UpdateRoleBinding(originOperatorContext(superAdmin), "tenant-1", "operator-1", binding)
			if !errors.Is(err, want) {
				t.Fatalf("UpdateRoleBinding error=%v, want %v", err, want)
			}
			if scenario.execCount != 0 {
				t.Fatalf("guard rejection caused %d writes", scenario.execCount)
			}
		})
	}
}

func originBindingFixture(origin string) (*roleBindingRepositoryScenario, domain.RoleBinding) {
	scenario := &roleBindingRepositoryScenario{
		roles: map[string]roleModel{
			"role-original": {ID: "role-original", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-original", RoleType: "PLATFORM", Status: domain.StatusActive},
			"role-other":    {ID: "role-other", TenantID: "tenant-1", ApplicationID: "app-platform", Code: "role-other", RoleType: "PLATFORM", Status: domain.StatusActive},
		},
		existingBinding: &roleBindingModel{ID: "binding-1", TenantID: "tenant-1", ApplicationID: "app-platform", RoleID: "role-original", SubjectType: "POSITION", SubjectID: "position-1", ScopeType: "TENANT", GrantOrigin: origin, Status: domain.StatusActive, Version: 3},
	}
	binding := domain.RoleBinding{ID: "binding-1", Role: domain.Reference{ID: "role-original"}, SubjectType: "POSITION", Subject: domain.Reference{ID: "position-1"}, ScopeType: "TENANT", Status: domain.StatusActive, Version: 3,
		// 请求不可信：MANUAL 标签不能覆盖持久化的归属信息。
		GrantOrigin: "MANUAL"}
	return scenario, binding
}

func mutateOriginBinding(binding *domain.RoleBinding, change string) {
	switch change {
	case "replace_role":
		binding.Role.ID = "role-other"
	case "move_subject":
		binding.SubjectType, binding.Subject.ID = "USER", "user-2"
	case "disable":
		binding.Status = domain.StatusDisabled
	case "change_scope":
		scopeID := "org-2"
		binding.ScopeType, binding.ScopeID = "ORG_UNIT", &scopeID
	case "change_expiry":
		expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		binding.ExpiresAt = &expires
	}
}

func originOperatorContext(superAdmin bool) context.Context {
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"}, Account: authctx.ReferenceName{ID: "account-1"}}
	if superAdmin {
		principal.Roles = []authctx.ReferenceName{{Code: domain.SuperAdminRoleCode}}
	}
	return authctx.WithPrincipal(context.Background(), principal)
}

// 仅替换 SQL 传输层；UpdateRoleBinding、委派、引用、
// 初始化管理员保护和版本逻辑均执行生产代码。
type originBindingDriver struct {
	scenario  *roleBindingRepositoryScenario
	bootstrap bool
}

type originBindingConn struct {
	*roleBindingTestConn
	bootstrap bool
}

func (d originBindingDriver) Open(string) (driver.Conn, error) {
	return &originBindingConn{roleBindingTestConn: &roleBindingTestConn{scenario: d.scenario}, bootstrap: d.bootstrap}, nil
}

func newOriginBindingRepository(t *testing.T, scenario *roleBindingRepositoryScenario, bootstrap bool) *GORMRepository {
	t.Helper()
	name := fmt.Sprintf("authorization-origin-test-%d", atomic.AddUint64(&roleBindingTestDriverCounter, 1))
	sql.Register(name, originBindingDriver{scenario: scenario, bootstrap: bootstrap})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func (c *originBindingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "iam_bootstrap_state"):
		rows := &roleBindingTestRows{columns: []string{"tenant_id", "first_super_admin_user_id", "first_super_admin_account_id"}}
		if c.bootstrap {
			rows.values = [][]driver.Value{{"tenant-1", "bootstrap-user", "bootstrap-account"}}
		}
		return rows, nil
	case strings.Contains(query, "FROM `iam_tenant`"):
		return &roleBindingTestRows{columns: []string{"code"}, values: [][]driver.Value{{"secondary-tenant"}}}, nil
	case c.bootstrap && strings.Contains(query, "SELECT status, deleted_at FROM `iam_user`"):
		return &roleBindingTestRows{columns: []string{"status", "deleted_at"}, values: [][]driver.Value{{"ACTIVE", nil}}}, nil
	case c.bootstrap && strings.Contains(query, "SELECT user_id, status FROM `iam_account`"):
		return &roleBindingTestRows{columns: []string{"user_id", "status"}, values: [][]driver.Value{{"bootstrap-user", "ACTIVE"}}}, nil
	case c.bootstrap && (strings.Contains(query, "FROM authz_role_binding AS binding JOIN authz_role AS role") || strings.Contains(query, "FROM authz_role AS role JOIN platform_application")):
		return singleInt64Rows("count", 1), nil
	case strings.Contains(query, "FROM `authz_role_binding`"):
		b := c.scenario.existingBinding
		return &roleBindingTestRows{columns: []string{"id", "tenant_id", "application_id", "role_id", "subject_type", "subject_id", "scope_type", "scope_id", "status", "version", "grant_origin", "valid_until"}, values: [][]driver.Value{{b.ID, b.TenantID, b.ApplicationID, b.RoleID, b.SubjectType, b.SubjectID, b.ScopeType, b.ScopeID, b.Status, int64(b.Version), b.GrantOrigin, nullableTime(b.ValidUntil)}}}, nil
	default:
		return c.roleBindingTestConn.QueryContext(ctx, query, args)
	}
}
