package infrastructure

import (
	"bytes"
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

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 密码校验完成与会话落库之间，管理员可能重置了密码。旧实现只按"账号存在活动凭据"
// 建立会话：重置后的新凭据同样满足条件，于是旧密码的登录请求仍能拿到有效会话。
// 会话必须绑定刚刚被验证过的那条凭据（按 password_hash 比对）。

type staleCredentialScenario struct {
	currentHash []byte
	queries     []string
	args        [][]driver.Value
}

type staleCredentialDriver struct{ scenario *staleCredentialScenario }
type staleCredentialConn struct{ scenario *staleCredentialScenario }
type staleCredentialTx struct{}

type staleCredentialResult int64

func (result staleCredentialResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result staleCredentialResult) RowsAffected() (int64, error) { return int64(result), nil }

type staleCredentialRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *staleCredentialRows) Columns() []string { return rows.columns }
func (*staleCredentialRows) Close() error           { return nil }

func (rows *staleCredentialRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

func (d *staleCredentialDriver) Open(string) (driver.Conn, error) {
	return &staleCredentialConn{scenario: d.scenario}, nil
}
func (*staleCredentialConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepared statements are not supported")
}
func (*staleCredentialConn) Close() error              { return nil }
func (*staleCredentialConn) Begin() (driver.Tx, error) { return staleCredentialTx{}, nil }
func (*staleCredentialConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return staleCredentialTx{}, nil
}
func (staleCredentialTx) Commit() error   { return nil }
func (staleCredentialTx) Rollback() error { return nil }

func (*staleCredentialConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return staleCredentialResult(1), nil
}

func (c *staleCredentialConn) QueryContext(_ context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	args := make([]driver.Value, 0, len(values))
	hashMatches := false
	for _, value := range values {
		args = append(args, value.Value)
		if raw, ok := value.Value.([]byte); ok && bytes.Equal(raw, c.scenario.currentHash) {
			hashMatches = true
		}
	}
	c.scenario.queries = append(c.scenario.queries, query)
	c.scenario.args = append(c.scenario.args, args)

	lowered := strings.ToLower(query)
	if strings.Contains(lowered, "from `iam_account`") || strings.Contains(lowered, "from iam_account") {
		if !hashMatches {
			// 密码已被重置：不存在"刚刚验证过的那条凭据"，登录必须失败关闭。
			return &staleCredentialRows{columns: []string{"id"}}, nil
		}
		return &staleCredentialRows{columns: []string{"id"}, values: [][]driver.Value{{"account-1"}}}, nil
	}
	return &staleCredentialRows{columns: []string{"id"}}, nil
}

var staleCredentialDriverCounter uint64

func newStaleCredentialRepository(t *testing.T, scenario *staleCredentialScenario) *GORMRepository {
	t.Helper()
	driverName := fmt.Sprintf("identity-stale-credential-%d", atomic.AddUint64(&staleCredentialDriverCounter, 1))
	sql.Register(driverName, &staleCredentialDriver{scenario: scenario})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open stale credential test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open stale credential gorm database: %v", err)
	}
	repository, err := NewGORMRepository(database)
	if err != nil {
		t.Fatalf("NewGORMRepository: %v", err)
	}
	return repository
}

func TestRecordSuccessfulPasswordVerificationRequiresVerifiedCredential(t *testing.T) {
	scenario := &staleCredentialScenario{currentHash: []byte("current-hash")}
	repository := newStaleCredentialRepository(t, scenario)

	err := repository.RecordSuccessfulPasswordVerification(context.Background(), domain.LoginAccount{
		TenantID: "tenant-1", AccountID: "account-1",
		PasswordHash: []byte("old-hash"), CredentialStatus: domain.StatusActive,
	}, time.Now().UTC())
	if !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("重置后按旧密码落库 error = %v, want ErrUnauthenticated", err)
	}
	if len(scenario.queries) == 0 || !strings.Contains(strings.ToLower(scenario.queries[0]), "password_hash") {
		t.Fatalf("凭据锁定语句必须按 password_hash 绑定刚刚验证的凭据: %v", scenario.queries)
	}
}

func TestCreateSessionRejectsSessionWhenPasswordChangedAfterVerification(t *testing.T) {
	scenario := &staleCredentialScenario{currentHash: []byte("current-hash")}
	repository := newStaleCredentialRepository(t, scenario)

	err := repository.CreateSession(context.Background(), domain.LoginAccount{
		TenantID: "tenant-1", UserID: "user-1", AccountID: "account-1",
		PasswordHash: []byte("old-hash"), CredentialStatus: domain.StatusActive,
	}, domain.Session{ID: "session-1", TenantID: "tenant-1", AccountID: "account-1", CreatedAt: time.Now().UTC()},
		30*time.Minute, false)
	if !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("密码重置后建立会话 error = %v, want ErrUnauthenticated", err)
	}
	if len(scenario.queries) == 0 || !strings.Contains(strings.ToLower(scenario.queries[0]), "password_hash") {
		t.Fatalf("账号锁定语句必须校验刚刚验证过的密码摘要: %v", scenario.queries)
	}
}

// OIDC 登录没有本地密码：不得因为没有密码凭据而拒绝建立会话。
func TestCreateSessionDoesNotRequirePasswordCredentialForOIDCLogin(t *testing.T) {
	scenario := &staleCredentialScenario{currentHash: []byte("current-hash")}
	repository := newStaleCredentialRepository(t, scenario)

	_ = repository.CreateSession(context.Background(), domain.LoginAccount{
		TenantID: "tenant-1", UserID: "user-1", AccountID: "account-1",
	}, domain.Session{ID: "session-1", TenantID: "tenant-1", AccountID: "account-1", CreatedAt: time.Now().UTC()},
		30*time.Minute, false)

	if len(scenario.queries) == 0 {
		t.Fatal("OIDC 登录必须执行账号锁定查询")
	}
	if strings.Contains(strings.ToLower(scenario.queries[0]), "password_hash") {
		t.Fatalf("OIDC 登录不应附带本地密码条件: %v", scenario.queries[0])
	}
}
