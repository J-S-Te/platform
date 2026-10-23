package infrastructure_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
)

const approvalExceptionTenant = "tenant-approval"

// approvalExceptionTestDatabase 在专用测试库上执行完整迁移链并清空审批/outbox 表。
// 未配置 PLATFORM_IDENTITY_TEST_DSN 时跳过；该 DSN 必须指向专用空数据库。
func approvalExceptionTestDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_IDENTITY_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_IDENTITY_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Run(context.Background(), database, migrations.Files); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	for _, table := range []string{"iam_personnel_approval_exception_outbox", "iam_personnel_change_transition", "iam_personnel_change_request"} {
		if err := database.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return database
}

func seedApprovalExceptionRequest(t *testing.T, database *gorm.DB, id, status string) application.PersonnelChangeRequest {
	t.Helper()
	now := time.Now().UTC()
	if err := database.Exec(`INSERT INTO iam_personnel_change_request
		(id, tenant_id, user_id, change_type, status, reason, effective_at, submitted_by, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, approvalExceptionTenant, "user-approval", "TRANSFER", status, "业务调整", now, "applicant-approval", 1, now, now).Error; err != nil {
		t.Fatalf("seed personnel change request: %v", err)
	}
	return application.PersonnelChangeRequest{
		ID: id, TenantID: approvalExceptionTenant, UserID: "user-approval", ChangeType: "TRANSFER",
		Status: status, SubmittedBy: "applicant-approval", Version: 1,
	}
}

func approvalExceptionEvent(request application.PersonnelChangeRequest, eventID string, occurredAt time.Time) application.PersonnelChangeAuditEvent {
	return application.PersonnelChangeAuditEvent{
		EventID: eventID, TenantID: request.TenantID, OperatorID: "applicant-approval", RequestID: request.ID,
		ToStatus: "SCHEDULED", SubmittedBy: "applicant-approval", SubjectUserID: "user-approval",
		Reason: "唯一在岗审批人，已按例外流程复核", RuleHit: "SELF_APPROVAL_EXCEPTION",
		ApprovalReference: "APR-1", OccurredAt: occurredAt,
	}
}

type approvalExceptionOutboxRow struct {
	Status          string
	Attempts        uint
	ExceptionReason string
	LastError       *string
	DeliveredAt     *time.Time
	NextAttemptAt   time.Time
}

func readApprovalExceptionOutbox(t *testing.T, database *gorm.DB, eventID string) approvalExceptionOutboxRow {
	t.Helper()
	var row approvalExceptionOutboxRow
	if err := database.Table("iam_personnel_approval_exception_outbox").Where("id = ?", eventID).Take(&row).Error; err != nil {
		t.Fatalf("read outbox row %s: %v", eventID, err)
	}
	return row
}

func readPersonnelChangeStatus(t *testing.T, database *gorm.DB, requestID string) string {
	t.Helper()
	var status string
	if err := database.Table("iam_personnel_change_request").Select("status").Where("id = ?", requestID).Scan(&status).Error; err != nil {
		t.Fatalf("read personnel change status: %v", err)
	}
	return status
}

// 审批状态推进与例外原因/审计 outbox 必须在同一事务提交，事务成功后两者都能读到。
func TestApprovalExceptionOutboxCommitsWithStatusOnMySQL(t *testing.T) {
	database := approvalExceptionTestDatabase(t)
	repository := infrastructure.NewPersonnelChangeGORMRepository(database)
	now := time.Now().UTC()
	request := seedApprovalExceptionRequest(t, database, "request-approval-1", "PENDING_APPROVAL")
	event := approvalExceptionEvent(request, "audit-approval-1", now)

	updated, err := repository.UpdateStatusWithApprovalException(context.Background(), request, "SCHEDULED", "APR-1", "applicant-approval", now, event)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "SCHEDULED" {
		t.Fatalf("status = %s, want SCHEDULED", updated.Status)
	}
	if status := readPersonnelChangeStatus(t, database, request.ID); status != "SCHEDULED" {
		t.Fatalf("stored status = %s, want SCHEDULED", status)
	}
	row := readApprovalExceptionOutbox(t, database, event.EventID)
	if row.Status != application.ApprovalExceptionOutboxPending || row.ExceptionReason != event.Reason || row.Attempts != 0 || row.DeliveredAt != nil {
		t.Fatalf("outbox row = %+v, want a PENDING row carrying the exception reason", row)
	}
}

// outbox 写入失败时状态推进必须回滚：不能出现审批已生效却丢失例外原因/审计事实。
func TestApprovalExceptionOutboxFailureRollsBackStatusOnMySQL(t *testing.T) {
	database := approvalExceptionTestDatabase(t)
	repository := infrastructure.NewPersonnelChangeGORMRepository(database)
	now := time.Now().UTC()

	first := seedApprovalExceptionRequest(t, database, "request-approval-a", "PENDING_APPROVAL")
	event := approvalExceptionEvent(first, "audit-approval-duplicate", now)
	if _, err := repository.UpdateStatusWithApprovalException(context.Background(), first, "SCHEDULED", "APR-1", "applicant-approval", now, event); err != nil {
		t.Fatalf("seed first approval: %v", err)
	}

	second := seedApprovalExceptionRequest(t, database, "request-approval-b", "PENDING_APPROVAL")
	duplicate := approvalExceptionEvent(second, event.EventID, now)
	if _, err := repository.UpdateStatusWithApprovalException(context.Background(), second, "SCHEDULED", "APR-2", "applicant-approval", now, duplicate); err == nil {
		t.Fatal("duplicate outbox event id must fail the transaction")
	}
	if status := readPersonnelChangeStatus(t, database, second.ID); status != "PENDING_APPROVAL" {
		t.Fatalf("status = %s, want the failed transaction to leave PENDING_APPROVAL", status)
	}
	var rows int64
	if err := database.Table("iam_personnel_approval_exception_outbox").Where("request_id = ?", second.ID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("outbox rows for the rolled back request = %d, want 0", rows)
	}
}

type approvalExceptionTestClock struct{ now time.Time }

func (clock approvalExceptionTestClock) Now() time.Time { return clock.now }

type approvalExceptionAuditSinkStub struct {
	err    error
	events []application.PersonnelChangeAuditEvent
}

func (sink *approvalExceptionAuditSinkStub) RecordApprovalException(_ context.Context, event application.PersonnelChangeAuditEvent) error {
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

// 审计端不可用时 outbox 保留待投递与重试时间；恢复后用同一事件 ID 投递并标记完成。
func TestApprovalExceptionOutboxDispatcherRetriesOnMySQL(t *testing.T) {
	database := approvalExceptionTestDatabase(t)
	repository := infrastructure.NewPersonnelChangeGORMRepository(database)
	// next_attempt_at 是 DATETIME(3)，固定时钟必须与存储精度一致。
	// 纳秒时钟写入后可能向上舍入，原值再查询 <= now 会偶发认为事件尚未到期。
	now := time.Now().UTC().Truncate(time.Millisecond)
	request := seedApprovalExceptionRequest(t, database, "request-approval-dispatch", "PENDING_APPROVAL")
	event := approvalExceptionEvent(request, "audit-approval-dispatch", now)
	if _, err := repository.UpdateStatusWithApprovalException(context.Background(), request, "SCHEDULED", "APR-1", "applicant-approval", now, event); err != nil {
		t.Fatal(err)
	}
	initial := readApprovalExceptionOutbox(t, database, event.EventID)
	if !initial.NextAttemptAt.Equal(now) {
		t.Fatalf("fixture due time = %s, clock = %s; expected identical millisecond precision", initial.NextAttemptAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}

	failingSink := &approvalExceptionAuditSinkStub{err: errors.New("audit endpoint unavailable")}
	dispatcher, err := application.NewApprovalExceptionDispatcher(repository, failingSink, approvalExceptionTestClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	report, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 1 || report.Delivered != 0 || report.Failed != 1 {
		t.Fatalf("report = %+v, want one failed delivery", report)
	}
	row := readApprovalExceptionOutbox(t, database, event.EventID)
	if row.Status != application.ApprovalExceptionOutboxPending || row.Attempts != 1 || row.DeliveredAt != nil {
		t.Fatalf("outbox after failure = %+v, want PENDING with one attempt", row)
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "audit endpoint unavailable") {
		t.Fatalf("last error = %v, want the delivery error recorded", row.LastError)
	}
	if !row.NextAttemptAt.After(now) {
		t.Fatalf("retry time = %s must be after the failed attempt", row.NextAttemptAt.Format(time.RFC3339Nano))
	}
	// 不推进固定时钟时，退避中的事件仍不可投递。
	beforeRetry, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil || beforeRetry.Inspected != 0 || beforeRetry.Delivered != 0 || beforeRetry.Failed != 0 {
		t.Fatalf("dispatch before retry time = %+v, err=%v, want no attempt", beforeRetry, err)
	}

	successSink := &approvalExceptionAuditSinkStub{}
	retryClock := approvalExceptionTestClock{now: now.Add(time.Minute)}
	dispatcher, err = application.NewApprovalExceptionDispatcher(repository, successSink, retryClock)
	if err != nil {
		t.Fatal(err)
	}
	report, err = dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 1 || report.Delivered != 1 || report.Failed != 0 {
		t.Fatalf("retry report = %+v, want one delivered event", report)
	}
	if len(successSink.events) != 1 || successSink.events[0].EventID != event.EventID {
		t.Fatalf("delivered events = %+v, want the stable outbox event id", successSink.events)
	}
	row = readApprovalExceptionOutbox(t, database, event.EventID)
	if row.Status != application.ApprovalExceptionOutboxDelivered || row.DeliveredAt == nil {
		t.Fatalf("outbox after retry = %+v, want DELIVERED", row)
	}

	// 已完成事件不再重复投递。
	report, err = dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 0 || len(successSink.events) != 1 {
		t.Fatalf("repeat dispatch = %+v events=%d, want no duplicate delivery", report, len(successSink.events))
	}
}
