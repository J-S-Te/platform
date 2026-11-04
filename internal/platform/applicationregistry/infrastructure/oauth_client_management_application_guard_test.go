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

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 安全审查 H-1 的写入面复核：创建客户端与整体替换 scope 都必须在同一事务内
// 读取并锁定父应用（code / owner_user_id），在写入任何行之前失败关闭。

type oauthClientGuardScenario struct {
	applicationCode string
	ownerUserID     string
	execs           []string
}

type oauthClientGuardResult struct{}

func (oauthClientGuardResult) LastInsertId() (int64, error) { return 1, nil }
func (oauthClientGuardResult) RowsAffected() (int64, error) { return 1, nil }

type oauthClientGuardRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *oauthClientGuardRows) Columns() []string { return rows.columns }
func (*oauthClientGuardRows) Close() error           { return nil }

func (rows *oauthClientGuardRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type oauthClientGuardDriver struct{ scenario *oauthClientGuardScenario }
type oauthClientGuardConn struct{ scenario *oauthClientGuardScenario }
type oauthClientGuardTx struct{}

func (oauthClientGuardTx) Commit() error   { return nil }
func (oauthClientGuardTx) Rollback() error { return nil }

func (d *oauthClientGuardDriver) Open(string) (driver.Conn, error) {
	return &oauthClientGuardConn{scenario: d.scenario}, nil
}
func (*oauthClientGuardConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*oauthClientGuardConn) Close() error              { return nil }
func (*oauthClientGuardConn) Begin() (driver.Tx, error) { return oauthClientGuardTx{}, nil }
func (*oauthClientGuardConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return oauthClientGuardTx{}, nil
}

func (c *oauthClientGuardConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.execs = append(c.scenario.execs, query)
	return oauthClientGuardResult{}, nil
}

func (c *oauthClientGuardConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	switch {
	case strings.Contains(normalized, "from platform_application_environment"):
		return &oauthClientGuardRows{
			columns: []string{"id", "tenant_id", "application_id", "status"},
			values:  [][]driver.Value{{"env-1", "tenant-1", "app-1", "ACTIVE"}},
		}, nil
	case strings.Contains(normalized, "from platform_application"):
		var owner driver.Value
		if c.scenario.ownerUserID != "" {
			owner = c.scenario.ownerUserID
		}
		return &oauthClientGuardRows{
			columns: []string{"id", "tenant_id", "code", "owner_user_id", "status"},
			values:  [][]driver.Value{{"app-1", "tenant-1", c.scenario.applicationCode, owner, "ACTIVE"}},
		}, nil
	case strings.Contains(normalized, "from platform_oauth_client ") || strings.HasSuffix(normalized, "from platform_oauth_client"):
		now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
		return &oauthClientGuardRows{
			columns: []string{"id", "tenant_id", "application_id", "environment_id", "client_id", "client_name", "client_type", "token_auth_method", "access_token_ttl_seconds", "refresh_token_ttl_seconds", "require_pkce", "status", "version", "created_at", "created_by", "updated_at", "updated_by"},
			values: [][]driver.Value{{
				"client-1", "tenant-1", "app-1", "env-1", "console-client", "控制台客户端", "public", "none",
				uint64(900), uint64(0), false, "ACTIVE", uint64(1), now, nil, now, nil,
			}},
		}, nil
	default:
		return &oauthClientGuardRows{columns: []string{"id"}}, nil
	}
}

var oauthClientGuardCounter uint64

func newOAuthClientGuardRepository(t *testing.T, scenario *oauthClientGuardScenario) *OAuthClientManagementRepository {
	t.Helper()
	name := fmt.Sprintf("oauth-client-guard-%d", atomic.AddUint64(&oauthClientGuardCounter, 1))
	sql.Register(name, &oauthClientGuardDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open scripted OAuth client database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open scripted gorm database: %v", err)
	}
	repository, err := NewOAuthClientManagementRepository(database)
	if err != nil {
		t.Fatalf("NewOAuthClientManagementRepository: %v", err)
	}
	return repository
}

func oauthClientGuardContext(tenantID, userID string, roleCodes, permissionCodes []string) context.Context {
	roles := make([]authctx.ReferenceName, 0, len(roleCodes))
	for _, code := range roleCodes {
		roles = append(roles, authctx.ReferenceName{Code: code})
	}
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:          authctx.ReferenceName{ID: tenantID},
		User:            authctx.ReferenceName{ID: userID},
		Roles:           roles,
		PermissionCodes: permissionCodes,
	})
}

func TestCreateOAuthClientRejectsReservedCatalogSyncScopeOnPlatformApplication(t *testing.T) {
	scenario := &oauthClientGuardScenario{applicationCode: "platform"}
	repository := newOAuthClientGuardRepository(t, scenario)
	// 与操作者无关：即使调用者是平台超级管理员，控制面应用的客户端也不得保留
	// 目录同步 scope。
	ctx := oauthClientGuardContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil)

	_, err := repository.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1", OperatorID: "operator-1",
		ClientID: "console-client", Scopes: []string{application.OAuthClientReservedCatalogSyncScope},
	}, "client-1", nil, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
	if !errors.Is(err, application.ErrManagementValidation) {
		t.Fatalf("reserved catalog sync scope must be rejected, got %v", err)
	}
	if len(scenario.execs) != 0 {
		t.Fatalf("rejected client creation must not write anything: %#v", scenario.execs)
	}
}

func TestGetOAuthClientParentApplicationReadsOwnershipAttributes(t *testing.T) {
	scenario := &oauthClientGuardScenario{applicationCode: "contract_management", ownerUserID: "owner-1"}
	repository := newOAuthClientGuardRepository(t, scenario)

	parent, err := repository.GetOAuthClientParentApplication(context.Background(), "tenant-1", "app-1")
	if err != nil {
		t.Fatalf("read parent application: %v", err)
	}
	if parent.Code != "contract_management" || parent.OwnerUserID != "owner-1" {
		t.Fatalf("parent application attributes = %#v, want code/owner", parent)
	}
}

func TestReplaceOAuthClientScopesRechecksParentApplication(t *testing.T) {
	cases := []struct {
		name            string
		applicationCode string
		roles           []string
		scopes          []string
		want            error
	}{
		{
			name:            "platform application rejects non-super administrator",
			applicationCode: "platform", roles: []string{"platform-security-admin"},
			scopes: []string{"openid"}, want: application.ErrManagementForbidden,
		},
		{
			name:            "platform application rejects reserved catalog sync scope even for super administrator",
			applicationCode: "platform", roles: []string{"platform-super-admin"},
			scopes: []string{"openid", application.OAuthClientReservedCatalogSyncScope}, want: application.ErrManagementValidation,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scenario := &oauthClientGuardScenario{applicationCode: testCase.applicationCode}
			repository := newOAuthClientGuardRepository(t, scenario)
			ctx := oauthClientGuardContext("tenant-1", "operator-1", testCase.roles, nil)

			_, err := repository.ReplaceOAuthClientScopes(ctx, application.OAuthClientScopesUpdateInput{
				TenantID: "tenant-1", OAuthClientID: "client-1", OperatorID: "operator-1",
				Scopes: testCase.scopes, Version: 1,
			}, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("scope replacement must fail with %v, got %v", testCase.want, err)
			}
			if len(scenario.execs) != 0 {
				t.Fatalf("rejected scope replacement must not write anything: %#v", scenario.execs)
			}
		})
	}
}

func TestCreateOAuthClientWritesForTenantApplication(t *testing.T) {
	scenario := &oauthClientGuardScenario{applicationCode: "contract_management", ownerUserID: "operator-1"}
	repository := newOAuthClientGuardRepository(t, scenario)
	ctx := oauthClientGuardContext("tenant-1", "operator-1", nil, nil)

	view, err := repository.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1", OperatorID: "operator-1",
		ClientID: "console-client", ClientName: "控制台客户端", ClientType: "public", TokenAuthMethod: "none",
		AccessTokenTTLSeconds: 900, GrantTypes: nil, Scopes: []string{"openid"}, RedirectURIs: nil,
	}, "client-1", nil, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("tenant application client creation must keep working: %v", err)
	}
	if view.ID != "client-1" {
		t.Fatalf("created client view = %#v, want client-1", view)
	}
	var inserted bool
	for _, statement := range scenario.execs {
		if strings.Contains(strings.ToLower(statement), "insert into") && strings.Contains(strings.ToLower(statement), "platform_oauth_client") {
			inserted = true
		}
	}
	if !inserted {
		t.Fatalf("successful creation did not insert the OAuth client: %#v", scenario.execs)
	}
}
