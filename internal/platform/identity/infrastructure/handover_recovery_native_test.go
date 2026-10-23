package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 原生 SQL 边界夹具，不使用 DSN 或在线数据库。
type handoverRecoveryDriver struct{ scenario *handoverRecoveryScenario }
type handoverRecoveryScenario struct {
	itemUpdated, receiptUpdated bool
	cancelling                  bool
	itemState, receiptState     string
	rediscoverManual            bool
}
type handoverRecoveryConn struct{ scenario *handoverRecoveryScenario }
type handoverRecoveryTx struct{}

func (d handoverRecoveryDriver) Open(string) (driver.Conn, error) {
	return &handoverRecoveryConn{d.scenario}, nil
}
func (c *handoverRecoveryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *handoverRecoveryConn) Close() error              { return nil }
func (c *handoverRecoveryConn) Begin() (driver.Tx, error) { return handoverRecoveryTx{}, nil }
func (handoverRecoveryTx) Commit() error                  { return nil }
func (handoverRecoveryTx) Rollback() error                { return nil }
func (c *handoverRecoveryConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "iam_personnel_handover_batch"):
		var cancelled driver.Value
		if c.scenario.cancelling {
			cancelled = time.Now().UTC()
		}
		return &membershipVersionRows{columns: []string{"id", "status", "cancel_requested_at"}, values: [][]driver.Value{{"batch-1", "FROZEN", cancelled}}}, nil
	case strings.Contains(q, "iam_personnel_handover_item"):
		return &membershipVersionRows{columns: []string{"id", "batch_id", "system_code", "current_owner_id", "status", "version", "idempotency_key", "attempts"}, values: [][]driver.Value{{"item-1", "batch-1", "contract", "leaver", "PENDING", int64(2), "handover:batch-1:item-1", int64(1)}}}, nil
	case strings.Contains(q, "iam_personnel_handover_receipt"):
		return &membershipVersionRows{columns: []string{"id", "tenant_id", "batch_id", "item_id", "system_code", "idempotency_key", "status"}, values: [][]driver.Value{{"receipt-1", "tenant-1", "batch-1", "item-1", "contract", "handover:batch-1:item-1", "RETRYABLE"}}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", q)
}
func (c *handoverRecoveryConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.HasPrefix(q, "INSERT INTO `iam_personnel_handover_item`"):
		return nil, &mysqldriver.MySQLError{Number: 1062, Message: "existing discovered resource"}
	case strings.HasPrefix(q, "INSERT INTO `iam_personnel_handover_receipt`"):
		return nil, &mysqldriver.MySQLError{Number: 1062, Message: "synthetic duplicate"}
	case strings.HasPrefix(q, "UPDATE `iam_personnel_handover_receipt`"):
		c.scenario.receiptUpdated = true
		for _, arg := range args {
			if arg.Value == "APPLIED" {
				c.scenario.receiptState = "APPLIED"
			}
		}
		return membershipVersionResult(1), nil
	case strings.HasPrefix(q, "UPDATE `iam_personnel_handover_item`"):
		c.scenario.itemUpdated = true
		if c.scenario.rediscoverManual {
			if !strings.Contains(q, "batch_id") || !strings.Contains(q, "attempts") || !strings.Contains(q, "COMPLETED") {
				return nil, errors.New("unsafe manual recovery scope")
			}
			for _, arg := range args {
				if arg.Value == "PENDING" {
					c.scenario.itemState = "PENDING"
				}
			}
		}
		for _, arg := range args {
			if arg.Value == "TRANSFERRED" {
				c.scenario.itemState = "TRANSFERRED"
			}
		}
		return membershipVersionResult(1), nil
	}
	return nil, fmt.Errorf("unexpected write: %s", q)
}

func TestReviewRediscoveryRestoresManuallyCompletedRemoteResponsibility(t *testing.T) {
	s := &handoverRecoveryScenario{rediscoverManual: true}
	repo, err := NewPersonnelHandoverGORMRepository(recoveryDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.InsertResponsibilityItems(context.Background(), "tenant-1", "batch-1", "request-1", "leaver", "", []application.HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "leaver", Version: 2}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.itemUpdated || s.itemState != "PENDING" {
		t.Fatalf("authoritative rediscovery left fake completion stuck: %+v", s)
	}
}
func recoveryDB(t *testing.T, s *handoverRecoveryScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("handover-native-%s", t.Name())
	sql.Register(name, handoverRecoveryDriver{s})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	g, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func TestReviewRetryableReceiptCanAdvanceToApplied(t *testing.T) {
	s := &handoverRecoveryScenario{}
	repo, err := NewPersonnelHandoverGORMRepository(recoveryDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.RecordReceipt(context.Background(), "tenant-1", "batch-1", 2, application.HandoverTransferReceipt{System: "contract", ItemID: "item-1", IdempotencyKey: "handover:batch-1:item-1", Status: application.HandoverReceiptApplied, AppliedVersion: 3}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.itemUpdated || !s.receiptUpdated {
		t.Fatalf("retryable receipt swallowed success: item=%v receipt=%v", s.itemUpdated, s.receiptUpdated)
	}
	if s.itemState != "TRANSFERRED" || s.receiptState != "APPLIED" {
		t.Fatalf("wrong persisted states: %+v", s)
	}
}

func TestReviewCancellationStopsRepositoryClaims(t *testing.T) {
	s := &handoverRecoveryScenario{cancelling: true}
	repo, err := NewPersonnelHandoverGORMRepository(recoveryDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	_, claimed, err := repo.ClaimNextItem(context.Background(), "tenant-1", "batch-1", "lease-1", time.Now().Add(time.Minute), time.Now())
	if !errors.Is(err, application.ErrConflict) || claimed || s.itemUpdated {
		t.Fatalf("cancelled batch claimed: %v %v %+v", claimed, err, s)
	}
}

func TestReviewInFlightTargetCannotBeChanged(t *testing.T) {
	s := &handoverRecoveryScenario{}
	repo, err := NewPersonnelHandoverGORMRepository(recoveryDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	err = repo.AssignTarget(context.Background(), "tenant-1", "item-1", "new-successor", time.Now())
	if !errors.Is(err, application.ErrConflict) || s.itemUpdated {
		t.Fatalf("in-flight target changed: %v", err)
	}
}
func TestReviewManualCheckerRejectsBatchResponsibility(t *testing.T) {
	s := &handoverRecoveryScenario{}
	checker, err := NewPersonnelHandoverGORMChecker(recoveryDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	_, err = checker.Complete(context.Background(), "tenant-1", "request-1", "item-1", "successor", "operator")
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("manual completion should fail before user lookup: %v", err)
	}
	if s.itemUpdated {
		t.Fatal("remote item manually updated")
	}
}
