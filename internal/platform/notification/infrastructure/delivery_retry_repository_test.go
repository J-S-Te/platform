package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 该进程内 SQL 驱动只实现本测试需要的通知投递语句，拒绝不符合契约的 SQL。
// 真实 GORM 仓储执行事务、扫描、CAS 更新；驱动中的状态可跨调用检查。
// 它不是 MySQL，不能替代真实 SKIP LOCKED/行锁并发验收。
type retrySQLStore struct {
	rows          map[string]deliveryModel
	readError     error
	writeError    error
	claimConflict bool
	commits       int
	rollbacks     int
	queries       int
}

type retrySQLConnector struct{ store *retrySQLStore }
type retrySQLDriver struct{ store *retrySQLStore }
type retrySQLConnection struct {
	store *retrySQLStore
	rows  map[string]deliveryModel
}
type retrySQLTransaction struct{ connection *retrySQLConnection }
type retrySQLRows struct {
	rows  [][]driver.Value
	index int
}

func (c retrySQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &retrySQLConnection{store: c.store}, nil
}
func (c retrySQLConnector) Driver() driver.Driver { return retrySQLDriver{c.store} }
func (d retrySQLDriver) Open(string) (driver.Conn, error) {
	return &retrySQLConnection{store: d.store}, nil
}
func (*retrySQLConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected delivery retry prepare")
}
func (*retrySQLConnection) Close() error { return nil }
func (c *retrySQLConnection) Begin() (driver.Tx, error) {
	c.rows = make(map[string]deliveryModel, len(c.store.rows))
	for id, row := range c.store.rows {
		c.rows[id] = row
	}
	return retrySQLTransaction{c}, nil
}
func (tx retrySQLTransaction) Commit() error {
	tx.connection.store.rows = tx.connection.rows
	tx.connection.store.commits++
	tx.connection.rows = nil
	return nil
}
func (tx retrySQLTransaction) Rollback() error {
	tx.connection.rows = nil
	tx.connection.store.rollbacks++
	return nil
}

func retryDue(row deliveryModel, now time.Time) bool {
	return (row.Status == "FAILED" && row.NextRetryAt != nil && !row.NextRetryAt.After(now)) ||
		(row.Status == "PROCESSING" && row.LockedUntil != nil && row.LockedUntil.Before(now))
}

func (c *retrySQLConnection) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.queries++
	if c.store.readError != nil {
		return nil, c.store.readError
	}
	if !strings.HasPrefix(query, "SELECT * FROM `notification_delivery`") || !strings.HasSuffix(query, "FOR UPDATE") && !strings.HasSuffix(query, "FOR UPDATE SKIP LOCKED") {
		return nil, fmt.Errorf("unexpected delivery SELECT: %s", query)
	}
	var selected []deliveryModel
	if strings.Contains(query, "SKIP LOCKED") {
		if !strings.Contains(query, "status = ? AND next_retry_at <= ?") ||
			!strings.Contains(query, "status = ? AND locked_until IS NOT NULL AND locked_until < ?") ||
			strings.Contains(query, "next_retry_at IS NULL") ||
			!strings.Contains(query, "ORDER BY next_retry_at ASC,id ASC LIMIT ?") {
			return nil, fmt.Errorf("unsafe retry due/lease/limit SELECT: %s", query)
		}
		tenant, offset := "", 0
		if len(args) == 6 {
			tenant, offset = args[0].Value.(string), 1
		} else if len(args) != 5 {
			return nil, fmt.Errorf("unexpected retry SELECT arguments: %v", args)
		}
		if args[offset].Value != "FAILED" || args[offset+2].Value != "PROCESSING" {
			return nil, fmt.Errorf("retry SELECT broadened allowed states: %v", args)
		}
		now, ok := args[offset+1].Value.(time.Time)
		if !ok || !now.Equal(args[offset+3].Value.(time.Time)) {
			return nil, errors.New("retry SELECT lost the current persisted due-time boundary")
		}
		for _, row := range c.rows {
			if (tenant == "" || tenant == row.TenantID) && retryDue(row, now) {
				selected = append(selected, row)
			}
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
		limit := int(args[len(args)-1].Value.(int64))
		if len(selected) > limit {
			selected = selected[:limit]
		}
	} else {
		if !strings.Contains(query, "WHERE id = ? AND tenant_id = ?") || len(args) != 3 {
			return nil, fmt.Errorf("unscoped completion SELECT: %s args=%v", query, args)
		}
		row, ok := c.rows[args[0].Value.(string)]
		if ok && row.TenantID == args[1].Value {
			selected = append(selected, row)
		}
	}
	output := &retrySQLRows{}
	for _, row := range selected {
		output.rows = append(output.rows, []driver.Value{row.ID, row.TenantID, row.Status, int64(row.AttemptCount), sqlTime(row.NextRetryAt), sqlTime(row.LockedUntil), sqlTime(row.DeliveredAt), row.LastError, row.UpdatedAt})
	}
	return output, nil
}

func (c *retrySQLConnection) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.store.writeError != nil {
		return nil, c.store.writeError
	}
	if !strings.HasPrefix(query, "UPDATE `notification_delivery` SET ") {
		return nil, fmt.Errorf("unexpected delivery write: %s", query)
	}
	parts := strings.SplitN(query, " WHERE ", 2)
	if len(parts) != 2 {
		return nil, errors.New("unbounded notification delivery UPDATE")
	}
	assignments := strings.Split(strings.TrimPrefix(parts[0], "UPDATE `notification_delivery` SET "), ",")
	values, consumed := map[string]any{}, 0
	for _, assignment := range assignments {
		pair := strings.SplitN(assignment, "=", 2)
		key := strings.Trim(pair[0], " `")
		if pair[1] == "?" {
			values[key] = args[consumed].Value
			consumed++
		} else if key != "attempt_count" || pair[1] != "attempt_count + 1" {
			return nil, fmt.Errorf("unexpected delivery expression: %s", assignment)
		}
	}
	whereArgs := args[consumed:]
	row, found := c.rows[whereArgs[0].Value.(string)]
	if !found {
		return driver.RowsAffected(0), nil
	}
	if strings.Contains(parts[1], "locked_until IS NOT NULL") {
		// 领取结果必须再次以记录 ID、记录租户和原到期条件做 CAS。
		offset := 1
		if whereArgs[1].Value != "FAILED" {
			if whereArgs[1].Value != row.TenantID {
				return driver.RowsAffected(0), nil
			}
			offset++
		} else if whereArgs[len(whereArgs)-1].Value != row.TenantID {
			return nil, fmt.Errorf("retry claim UPDATE lost persisted tenant: %s args=%v", query, whereArgs)
		}
		if !strings.Contains(parts[1], "tenant_id = ?") {
			return nil, fmt.Errorf("unscoped retry claim UPDATE: %s", query)
		}
		if c.store.claimConflict || !retryDue(row, whereArgs[offset+1].Value.(time.Time)) {
			return driver.RowsAffected(0), nil
		}
	} else {
		if !strings.Contains(parts[1], "id = ? AND tenant_id = ? AND status") || whereArgs[1].Value != row.TenantID {
			return driver.RowsAffected(0), nil
		}
		matched := false
		for _, status := range whereArgs[2:] {
			matched = matched || row.Status == status.Value
		}
		if !matched {
			return driver.RowsAffected(0), nil
		}
	}
	for key, value := range values {
		switch key {
		case "status":
			row.Status = value.(string)
		case "locked_until":
			row.LockedUntil = fromSQLTime(value)
		case "next_retry_at":
			row.NextRetryAt = fromSQLTime(value)
		case "delivered_at":
			row.DeliveredAt = fromSQLTime(value)
		case "last_error":
			row.LastError = value.(string)
		case "updated_at":
			row.UpdatedAt = value.(time.Time)
		default:
			return nil, fmt.Errorf("unexpected delivery update field: %s", key)
		}
	}
	if strings.Contains(parts[0], "`attempt_count`=attempt_count + 1") {
		row.AttemptCount++
	}
	c.rows[row.ID] = row
	return driver.RowsAffected(1), nil
}

func (*retrySQLRows) Columns() []string {
	return []string{"id", "tenant_id", "status", "attempt_count", "next_retry_at", "locked_until", "delivered_at", "last_error", "updated_at"}
}
func (*retrySQLRows) Close() error { return nil }
func (rows *retrySQLRows) Next(values []driver.Value) error {
	if rows.index >= len(rows.rows) {
		return io.EOF
	}
	copy(values, rows.rows[rows.index])
	rows.index++
	return nil
}

func sqlTime(value *time.Time) driver.Value {
	if value == nil {
		return nil
	}
	return *value
}
func fromSQLTime(value any) *time.Time {
	if value == nil {
		return nil
	}
	t := value.(time.Time)
	return &t
}

func retrySQLRepository(t *testing.T, store *retrySQLStore) *Repository {
	t.Helper()
	connection := sql.OpenDB(retrySQLConnector{store})
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func claimDueForTest(t *testing.T, r *Repository, limit int, lease, now time.Time) ([]domain.Delivery, error) {
	t.Helper()
	return r.ClaimDueDeliveries(context.Background(), limit, lease, now)
}

func TestBackgroundDeliveryClaimPersistsLeaseAndExcludesNotDueRows(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	store := &retrySQLStore{rows: map[string]deliveryModel{
		"a-due":     {ID: "a-due", TenantID: "tenant-a", Status: "FAILED", NextRetryAt: &now},
		"b-expired": {ID: "b-expired", TenantID: "tenant-b", Status: "PROCESSING", LockedUntil: &past},
		"future":    {ID: "future", TenantID: "tenant-a", Status: "FAILED", NextRetryAt: &future},
		"no-retry":  {ID: "no-retry", TenantID: "tenant-a", Status: "FAILED"},
		"active":    {ID: "active", TenantID: "tenant-a", Status: "PROCESSING", LockedUntil: &future},
		"delivered": {ID: "delivered", TenantID: "tenant-a", Status: "DELIVERED", NextRetryAt: &past},
	}}
	repository := retrySQLRepository(t, store)
	lease := now.Add(2 * time.Minute)
	claimed, err := claimDueForTest(t, repository, 10, lease, now)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed=%+v error=%v", claimed, err)
	}
	for _, id := range []string{"a-due", "b-expired"} {
		row := store.rows[id]
		if row.Status != "PROCESSING" || row.LockedUntil == nil || !row.LockedUntil.Equal(lease) {
			t.Fatalf("claim not committed for %s: %+v", id, row)
		}
	}
	if claimed[0].TenantID != "tenant-a" || claimed[1].TenantID != "tenant-b" {
		t.Fatalf("claim lost persisted tenants: %+v", claimed)
	}
	if again, err := claimDueForTest(t, repository, 10, lease, now); err != nil || len(again) != 0 {
		t.Fatalf("active leases must not be claimed again: %+v err=%v", again, err)
	}
	if _, err := repository.CompleteDelivery(context.Background(), "tenant-b", "a-due", now); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("wrong tenant completion error=%v", err)
	}
	if _, err := repository.CompleteDelivery(context.Background(), "tenant-a", "a-due", now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CompleteDelivery(context.Background(), "tenant-a", "a-due", now); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("delivered terminal must be idempotently protected: %v", err)
	}
	if store.rows["a-due"].Status != "DELIVERED" || store.rows["a-due"].LockedUntil != nil || store.rows["a-due"].NextRetryAt != nil {
		t.Fatal("completion must commit terminal state and clear retry lease")
	}
	if err := repository.FailDelivery(context.Background(), "tenant-b", "b-expired", "safe failure", future, now); err != nil {
		t.Fatal(err)
	}
	if got := store.rows["b-expired"]; got.Status != "FAILED" || got.AttemptCount != 1 || got.NextRetryAt == nil || !got.NextRetryAt.Equal(future) || got.LockedUntil != nil {
		t.Fatalf("failure state did not commit: %+v", got)
	}
}

func TestBackgroundDeliveryClaimRollsBackFailuresAndCASConflicts(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, failure := range []string{"read", "write", "cas"} {
		t.Run(failure, func(t *testing.T) {
			store := &retrySQLStore{rows: map[string]deliveryModel{"due": {ID: "due", TenantID: "tenant-a", Status: "FAILED", NextRetryAt: &now}}}
			switch failure {
			case "read":
				store.readError = errors.New("read failed")
			case "write":
				store.writeError = errors.New("write failed")
			case "cas":
				store.claimConflict = true
			}
			claimed, err := claimDueForTest(t, retrySQLRepository(t, store), 10, now.Add(2*time.Minute), now)
			if err == nil || len(claimed) != 0 || store.commits != 0 || store.rollbacks != 1 || store.rows["due"].Status != "FAILED" {
				t.Fatalf("failed claim must roll back without consuming work: rows=%+v claimed=%+v err=%v commits=%d rollbacks=%d", store.rows, claimed, err, store.commits, store.rollbacks)
			}
			if failure == "cas" && !errors.Is(err, application.ErrConflict) {
				t.Fatalf("CAS loss must remain a conflict: %v", err)
			}
		})
	}
}

func TestManualDeliveryClaimPreservesTenantScope(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	store := &retrySQLStore{rows: map[string]deliveryModel{
		"owned":   {ID: "owned", TenantID: "tenant-a", Status: "FAILED", NextRetryAt: &now},
		"foreign": {ID: "foreign", TenantID: "tenant-b", Status: "FAILED", NextRetryAt: &now},
	}}
	repository := retrySQLRepository(t, store)
	claimed, err := repository.ClaimFailedDeliveries(context.Background(), "tenant-a", 1, now.Add(2*time.Minute), now)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "owned" || store.rows["foreign"].Status != "FAILED" {
		t.Fatalf("manual claim crossed tenant boundary: claimed=%+v err=%v rows=%+v", claimed, err, store.rows)
	}
}
