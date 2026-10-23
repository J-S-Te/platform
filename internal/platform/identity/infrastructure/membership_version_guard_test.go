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

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 任职的"换组织 / 换岗位 / 换主次"分支曾直接用数据库当前版本关闭旧任职，完全忽略
// 请求携带的版本号：旧页面（或另一位管理员）可以覆盖后来发生的变更，且不会得到
// 版本冲突。乐观锁必须在任何写入之前按请求版本校验。
//
// 这里用脚本化驱动把数据库里的任职版本设为 5，请求版本设为 4，断言：
// 返回版本冲突，且没有执行任何写入语句。

type membershipVersionScenario struct {
	writes []string
}

type membershipVersionDriver struct{ scenario *membershipVersionScenario }
type membershipVersionConn struct{ scenario *membershipVersionScenario }
type membershipVersionTx struct{}

type membershipVersionResult int64

func (result membershipVersionResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result membershipVersionResult) RowsAffected() (int64, error) { return int64(result), nil }

type membershipVersionRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *membershipVersionRows) Columns() []string { return rows.columns }
func (*membershipVersionRows) Close() error           { return nil }

func (rows *membershipVersionRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

func (d *membershipVersionDriver) Open(string) (driver.Conn, error) {
	return &membershipVersionConn{scenario: d.scenario}, nil
}
func (*membershipVersionConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepared statements are not supported")
}
func (*membershipVersionConn) Close() error              { return nil }
func (*membershipVersionConn) Begin() (driver.Tx, error) { return membershipVersionTx{}, nil }
func (*membershipVersionConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return membershipVersionTx{}, nil
}
func (membershipVersionTx) Commit() error   { return nil }
func (membershipVersionTx) Rollback() error { return nil }

func (c *membershipVersionConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.writes = append(c.scenario.writes, query)
	return membershipVersionResult(1), nil
}

func (c *membershipVersionConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	lowered := strings.ToLower(query)
	switch {
	case strings.Contains(lowered, "from `iam_membership`") || strings.Contains(lowered, "from iam_membership"):
		now := time.Now().UTC()
		return &membershipVersionRows{
			columns: []string{"id", "tenant_id", "user_id", "org_unit_id", "position_id", "membership_type", "is_primary", "inherit_authorization", "valid_from", "valid_until", "status", "version", "created_at", "created_by", "updated_at", "updated_by"},
			values: [][]driver.Value{{
				"membership-1", "tenant-1", "user-1", "org-1", "position-1", domain.MembershipPrimary, true, true,
				nil, nil, domain.StatusActive, uint64(5), now, nil, now, nil,
			}},
		}, nil
	default:
		return &membershipVersionRows{columns: []string{"id"}}, nil
	}
}

var membershipVersionDriverCounter uint64

func TestUpdateMembershipRejectsStaleVersionBeforeChangingAssignments(t *testing.T) {
	scenario := &membershipVersionScenario{}
	driverName := fmt.Sprintf("identity-membership-version-%d", atomic.AddUint64(&membershipVersionDriverCounter, 1))
	sql.Register(driverName, &membershipVersionDriver{scenario: scenario})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open membership version test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open membership version gorm database: %v", err)
	}
	repository, err := NewGORMRepository(database)
	if err != nil {
		t.Fatalf("NewGORMRepository: %v", err)
	}

	status := domain.StatusActive
	_, err = repository.UpdateMembership(context.Background(), application.MembershipUpdateInput{
		MembershipCreateInput: application.MembershipCreateInput{
			TenantID: "tenant-1", OperatorID: "operator-1", UserID: "user-1",
			OrgUnitID: "org-2", PositionID: "position-2", MembershipType: domain.MembershipPrimary,
		},
		MembershipID: "membership-1",
		Status:       &status,
		Version:      4, // 数据库当前版本是 5
	})
	if !errors.Is(err, application.ErrVersionConflict) {
		t.Fatalf("UpdateMembership(旧版本换岗) error = %v, want ErrVersionConflict", err)
	}
	if len(scenario.writes) != 0 {
		t.Fatalf("旧版本请求不得写入任何数据，实际写入 %d 条: %v", len(scenario.writes), scenario.writes)
	}
}
