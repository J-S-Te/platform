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

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 安全（SEC-D3）：RotateRefreshToken 必须可完整走通，且每条语句的 SQL 占位符个数与
// 绑定变量个数一致。此前最后一步 UPDATE oauth_token_family 的 Where("id = ?") 漏传
// family.ID，生成 SQL 有 2 个占位符却只绑定 1 个变量，MySQL 执行期拒绝，refresh_token
// grant 每次轮换都在此回滚。脚本驱动逐条记录并比对占位符/参数个数，任何不匹配即失败。

type rotateStatement struct {
	query        string
	placeholders int
	args         int
}

type rotateScript struct {
	statements   []rotateStatement
	mismatches   []rotateStatement
	authorizedAt time.Time
	now          time.Time
}

func (script *rotateScript) record(query string, args int) {
	entry := rotateStatement{query: query, placeholders: strings.Count(query, "?"), args: args}
	script.statements = append(script.statements, entry)
	if entry.placeholders != entry.args {
		script.mismatches = append(script.mismatches, entry)
	}
}

func TestRotateRefreshTokenBindsEveryPlaceholder(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	script := &rotateScript{authorizedAt: now.Add(-24 * time.Hour), now: now}
	repository := newRotateRepositoryTest(t, script)

	var tokenHash [32]byte
	copy(tokenHash[:], "presented-refresh-token-digest-32b!")

	grant, err := repository.RotateRefreshToken(context.Background(), application.RotateRefreshTokenCommand{
		TokenHash: tokenHash,
		ClientID:  "client-1",
		Refresh: application.NewRefreshToken{
			ID:                   "refresh-2",
			ParentRefreshTokenID: "refresh-1",
			TokenHash:            [32]byte{9, 8, 7},
			TenantID:             "tenant-1",
			OAuthClientID:        "client-1",
			SessionID:            "session-1",
			AccountID:            "account-1",
			UserID:               "user-1",
			Scopes:               []string{"openid", "profile"},
			AuthorizedAt:         script.authorizedAt,
			IssuedAt:             now,
			ExpiresAt:            now.Add(30 * 24 * time.Hour),
		},
	}, now)
	if err != nil {
		t.Fatalf("RotateRefreshToken error = %v; statements=%+v", err, script.statements)
	}
	if grant.TenantID != "tenant-1" || grant.ClientID != "client-public" || grant.SessionID != "session-1" {
		t.Fatalf("grant = %+v", grant)
	}

	// 核心断言一：整条 refresh_token 轮换路径上没有任何「占位符≠参数个数」的语句。
	if len(script.mismatches) > 0 {
		t.Fatalf("占位符与绑定参数个数不一致的语句: %+v", script.mismatches)
	}

	// 核心断言二：族过期时间延长语句必须显式绑定 family.ID（SEC-D3 修复点）。
	var familyUpdate rotateStatement
	found := false
	for _, entry := range script.statements {
		if strings.HasPrefix(strings.TrimSpace(entry.query), "UPDATE") && strings.Contains(entry.query, "oauth_token_family") && strings.Contains(entry.query, "expires_at") {
			familyUpdate, found = entry, true
			break
		}
	}
	if !found {
		t.Fatalf("未执行 oauth_token_family 的 expires_at 延长语句; statements=%+v", script.statements)
	}
	if familyUpdate.placeholders != 2 || familyUpdate.args != 2 {
		t.Fatalf("族延长语句 占位符=%d 参数=%d, want 2/2: %s", familyUpdate.placeholders, familyUpdate.args, familyUpdate.query)
	}
	if !strings.Contains(familyUpdate.query, "id = ?") {
		t.Fatalf("族延长语句缺少 id 条件: %s", familyUpdate.query)
	}
}

func newRotateRepositoryTest(t *testing.T, script *rotateScript) *Repository {
	t.Helper()
	driverName := fmt.Sprintf("oidc-rotate-test-%d", atomic.AddUint64(&rotateScriptDriverCounter, 1))
	sql.Register(driverName, &rotateScriptDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open rotate test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open rotate GORM database: %v", err)
	}
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	return repository
}

var rotateScriptDriverCounter uint64

type rotateScriptDriver struct{ script *rotateScript }
type rotateScriptConn struct{ script *rotateScript }
type rotateScriptTx struct{}

type rotateScriptResult int64

// backtick 用于匹配 GORM 反引号包裹的表名，避免测试源码里出现原始反引号。
const backtick = "\x60"

func (result rotateScriptResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result rotateScriptResult) RowsAffected() (int64, error) { return int64(result), nil }

type rotateScriptRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (driver *rotateScriptDriver) Open(string) (driver.Conn, error) {
	return &rotateScriptConn{script: driver.script}, nil
}

func (connection *rotateScriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by rotate test driver")
}

func (*rotateScriptConn) Close() error { return nil }

func (*rotateScriptConn) Begin() (driver.Tx, error) { return rotateScriptTx{}, nil }
func (*rotateScriptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return rotateScriptTx{}, nil
}
func (rotateScriptTx) Commit() error   { return nil }
func (rotateScriptTx) Rollback() error { return nil }

func (connection *rotateScriptConn) ExecContext(_ context.Context, query string, values []driver.NamedValue) (driver.Result, error) {
	connection.script.record(query, len(values))
	return rotateScriptResult(1), nil
}

func (connection *rotateScriptConn) QueryContext(_ context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	connection.script.record(query, len(values))
	script := connection.script
	switch {
	case strings.Contains(query, "SELECT oauth_refresh_token.id"):
		// refreshByHash 的投影查询（带 FOR UPDATE 行锁）。
		return &rotateScriptRows{
			columns: []string{
				"id", "tenant_id", "oauth_client_id", "token_family_id", "parent_refresh_token_id",
				"token_hash", "issued_at", "expires_at", "used_at", "revoked_at", "revoke_reason", "status",
				"session_id", "account_id", "user_id", "scope", "authorized_at",
				"family_expires_at", "family_revoked_at", "family_status",
			},
			values: [][]driver.Value{{
				"refresh-1", "tenant-1", "client-1", "family-1", nil,
				make([]byte, 32), script.now.Add(-time.Hour), script.now.Add(time.Hour), nil, nil, "", domain.RefreshTokenStatusActive,
				"session-1", "account-1", "user-1", "openid profile", script.authorizedAt,
				script.now.Add(2 * time.Hour), nil, domain.TokenFamilyStatusActive,
			}},
		}, nil
	case strings.Contains(query, "FROM "+backtick+"oauth_token_family"+backtick):
		// 族行锁查询。
		return &rotateScriptRows{
			columns: []string{"id", "tenant_id", "oauth_client_id", "session_id", "account_id", "user_id", "scope", "authorized_at", "created_at", "expires_at", "revoked_at", "revoke_reason", "status"},
			values: [][]driver.Value{{
				"family-1", "tenant-1", "client-1", "session-1", "account-1", "user-1", "openid profile",
				script.authorizedAt, script.authorizedAt, script.now.Add(2 * time.Hour), nil, "", domain.TokenFamilyStatusActive,
			}},
		}, nil
	case strings.Contains(query, "FROM "+backtick+"platform_oauth_client"+backtick):
		return &rotateScriptRows{
			columns: []string{"id", "tenant_id", "client_id", "client_type", "token_auth_method", "access_token_ttl_seconds", "refresh_token_ttl_seconds", "require_pkce"},
			values:  [][]driver.Value{{"client-1", "tenant-1", "client-public", "CONFIDENTIAL", "client_secret_basic", int64(900), int64(86400), false}},
		}, nil
	case strings.Contains(query, "platform_oauth_grant_type"):
		return &rotateScriptRows{columns: []string{"count"}, values: [][]driver.Value{{int64(1)}}}, nil
	default:
		return &rotateScriptRows{columns: []string{"id"}}, nil
	}
}

func (rows *rotateScriptRows) Columns() []string { return rows.columns }
func (*rotateScriptRows) Close() error           { return nil }

func (rows *rotateScriptRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}
