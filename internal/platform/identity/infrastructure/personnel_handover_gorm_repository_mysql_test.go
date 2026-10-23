package infrastructure_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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

// handoverTestDatabase 在专用测试库上执行完整迁移链并清空交接控制表。
// 未配置 PLATFORM_IDENTITY_TEST_DSN 时跳过；该 DSN 必须指向专用空数据库。
func handoverTestDatabase(t *testing.T) *gorm.DB {
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
	for _, table := range []string{"iam_personnel_handover_receipt", "iam_personnel_handover_item", "iam_personnel_handover_system", "iam_personnel_handover_batch"} {
		if err := database.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return database
}

func TestHandoverPendingCompletionQueueBoundsAndTenantCorrelationOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	ctx := context.Background()
	now := time.Now().UTC()
	fixtures := []struct {
		tenant, personnelStatus, changeType, batchStatus string
		wrongTenant, wrongUser, cancelling               bool
		eligible                                         bool
	}{
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "FROZEN", eligible: true},
		{tenant: "queue-tenant-b", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "TRANSFERRING", eligible: true},
		{tenant: "queue-tenant-c", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "FROZEN", eligible: true},
		{tenant: "queue-tenant-a", personnelStatus: "SCHEDULED", changeType: "TERMINATION", batchStatus: "FROZEN"},
		{tenant: "queue-tenant-a", personnelStatus: "PENDING_HANDOVER", changeType: "TERMINATION", batchStatus: "FROZEN"},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TRANSFER", batchStatus: "FROZEN"},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "FROZEN", wrongTenant: true},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "FROZEN", wrongUser: true},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "COMPLETED"},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "CANCELLED"},
		{tenant: "queue-tenant-a", personnelStatus: "EXECUTED", changeType: "TERMINATION", batchStatus: "FROZEN", cancelling: true},
	}
	var eligible []application.HandoverBatch
	for i, fixture := range fixtures {
		requestID := fmt.Sprintf("queue-request-%02d", i)
		if err := database.Table("iam_personnel_change_request").Create(map[string]any{"id": requestID, "tenant_id": fixture.tenant, "user_id": "queue-leaver", "change_type": fixture.changeType, "status": fixture.personnelStatus, "reason": "completion queue fixture", "effective_at": now, "submitted_by": "queue-applicant", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := database.Exec("DELETE FROM iam_personnel_change_request WHERE id = ?", requestID).Error; err != nil {
				t.Errorf("clean queue request: %v", err)
			}
		})
		batchTenant, batchUser := fixture.tenant, "queue-leaver"
		if fixture.wrongTenant {
			batchTenant = "queue-wrong-tenant"
		}
		if fixture.wrongUser {
			batchUser = "queue-wrong-user"
		}
		batch, err := repository.EnsureBatch(ctx, batchTenant, requestID, batchUser, now)
		if err != nil {
			t.Fatal(err)
		}
		freezeHandoverBatch(t, repository, batch, now)
		if fixture.cancelling {
			if err := repository.RequestCancellation(ctx, batchTenant, batch.ID, now); err != nil {
				t.Fatal(err)
			}
		} else if fixture.batchStatus != "FROZEN" {
			if err := repository.CompleteBatch(ctx, batchTenant, batch.ID, fixture.batchStatus, now); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.eligible {
			eligible = append(eligible, batch)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	all, err := repository.ListPendingCompletionBatches(ctx, "", 100)
	if err != nil || len(all) != 3 {
		t.Fatalf("eligible queue=%+v err=%v", all, err)
	}
	for i := range eligible {
		if all[i].ID != eligible[i].ID || all[i].TenantID != eligible[i].TenantID {
			t.Fatalf("queue crossed request/tenant/user boundaries: %+v", all)
		}
	}
	first, err := repository.ListPendingCompletionBatches(ctx, "", 2)
	if err != nil || len(first) != 2 || first[0].ID != eligible[0].ID || first[1].ID != eligible[1].ID {
		t.Fatalf("bounded first page=%+v err=%v", first, err)
	}
	second, err := repository.ListPendingCompletionBatches(ctx, first[1].ID, 2)
	if err != nil || len(second) != 1 || second[0].ID != eligible[2].ID {
		t.Fatalf("cursor second page=%+v err=%v", second, err)
	}
	if end, err := repository.ListPendingCompletionBatches(ctx, second[0].ID, 2); err != nil || len(end) != 0 {
		t.Fatalf("cursor end=%+v err=%v", end, err)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := repository.ListPendingCompletionBatches(ctx, "", limit); !errors.Is(err, application.ErrValidation) {
			t.Fatalf("invalid bound %d accepted: %v", limit, err)
		}
	}
}

func handoverRepository(t *testing.T, database *gorm.DB) *infrastructure.PersonnelHandoverGORMRepository {
	t.Helper()
	repository, err := infrastructure.NewPersonnelHandoverGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func newHandoverBatch(t *testing.T, repository *infrastructure.PersonnelHandoverGORMRepository, now time.Time) application.HandoverBatch {
	t.Helper()
	batch, err := repository.EnsureBatch(context.Background(), "tenant-handover", "request-handover", "user-leaver", now)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

// freezeHandoverBatch 完成领取所需的真实系统检查点，不绕过 DISCOVERING guard。
func freezeHandoverBatch(t *testing.T, repository *infrastructure.PersonnelHandoverGORMRepository, batch application.HandoverBatch, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := repository.UpsertSystemState(ctx, batch.TenantID, batch.ID, "contract", application.HandoverSystemApplicable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkFrozen(ctx, batch.TenantID, batch.ID, "contract", now, 1); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteBatch(ctx, batch.TenantID, batch.ID, application.HandoverBatchFrozen, now); err != nil {
		t.Fatal(err)
	}
}

// seedActiveHandoverUser 仅为显式隔离库创建合成租户/人员，按创建顺序反向清理。
func seedActiveHandoverUser(t *testing.T, database *gorm.DB, tenantID, userID string) {
	t.Helper()
	now := time.Now().UTC()
	var tenants int64
	if err := database.Table("iam_tenant").Where("id = ?", tenantID).Count(&tenants).Error; err != nil {
		t.Fatal(err)
	}
	if tenants == 0 {
		if err := database.Table("iam_tenant").Create(map[string]any{"id": tenantID, "code": tenantID, "name": "Handover test tenant", "timezone": "UTC", "locale": "en-US", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := database.Exec("DELETE FROM iam_tenant WHERE id = ?", tenantID).Error; err != nil {
				t.Errorf("clean fixture tenant: %v", err)
			}
		})
	}
	if err := database.Table("iam_user").Create(map[string]any{"id": userID, "tenant_id": tenantID, "display_name": "Handover test user", "employment_status": "ACTIVE", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Exec("DELETE FROM iam_user WHERE tenant_id = ? AND id = ?", tenantID, userID).Error; err != nil {
			t.Errorf("clean fixture user: %v", err)
		}
	})
}

// seedResponsibility 返回本次真正新插入的责任项数量（重复发现应为 0）。
func seedResponsibility(t *testing.T, repository *infrastructure.PersonnelHandoverGORMRepository, batch application.HandoverBatch, resourceID string, now time.Time) int {
	t.Helper()
	inserted, err := repository.InsertResponsibilityItems(context.Background(), batch.TenantID, batch.ID, batch.RequestID, batch.UserID, "user-successor", []application.HandoverResponsibility{{
		System: "contract", ResourceType: "contract", ResourceID: resourceID, ResourceName: resourceID, OwnerUserID: batch.UserID, Category: "OWNER", Version: 7, UpdatedAt: now,
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	return inserted
}

func TestHandoverBatchAndItemsAreIdempotentOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	now := time.Now().UTC()

	first := newHandoverBatch(t, repository, now)
	second, err := repository.EnsureBatch(context.Background(), first.TenantID, first.RequestID, first.UserID, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("batch ids differ: %s vs %s", first.ID, second.ID)
	}
	if inserted := seedResponsibility(t, repository, first, "contract-1", now); inserted != 1 {
		t.Fatalf("first discovery inserted = %d, want 1", inserted)
	}
	if inserted := seedResponsibility(t, repository, first, "contract-1", now); inserted != 0 {
		t.Fatalf("repeated discovery inserted = %d, want 0 (idempotent)", inserted)
	}

	items, err := repository.ListItems(context.Background(), first.TenantID, first.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want the responsibility to be stored once", len(items))
	}
	if items[0].IdempotencyKey == "" || items[0].ExpectedVersion != 7 || items[0].Status != application.HandoverItemPending {
		t.Fatalf("stored item incomplete: %+v", items[0])
	}
}

func TestHandoverClaimUsesLeaseAndRetryScheduleOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	now := time.Now().UTC()
	batch := newHandoverBatch(t, repository, now)
	seedResponsibility(t, repository, batch, "contract-2", now)
	freezeHandoverBatch(t, repository, batch, now)

	claimed, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-1", now.Add(time.Minute), now)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v", ok, err)
	}
	if claimed.Attempts != 1 || claimed.LeaseID != "lease-1" {
		t.Fatalf("claim did not take the lease: %+v", claimed)
	}
	if _, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-2", now.Add(2*time.Minute), now); err != nil || ok {
		t.Fatalf("second claim while the lease is held = %v/%v, want none", ok, err)
	}
	later := now.Add(2 * time.Minute)
	reclaimed, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-3", later.Add(time.Minute), later)
	if err != nil || !ok {
		t.Fatalf("claim after lease expiry = %v/%v", ok, err)
	}
	if reclaimed.Attempts != 2 || reclaimed.LeaseID != "lease-3" {
		t.Fatalf("expired lease was not reclaimed: %+v", reclaimed)
	}
}

func TestHandoverReceiptIsIdempotentAndVersionCheckedOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	now := time.Now().UTC()
	batch := newHandoverBatch(t, repository, now)
	seedResponsibility(t, repository, batch, "contract-3", now)
	freezeHandoverBatch(t, repository, batch, now)
	claimed, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-1", now.Add(time.Minute), now)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v", ok, err)
	}
	receipt := application.HandoverTransferReceipt{
		System: "contract", ItemID: claimed.ID, IdempotencyKey: claimed.IdempotencyKey,
		Status: application.HandoverReceiptApplied, AppliedVersion: 8,
	}

	inserted, err := repository.RecordReceipt(context.Background(), batch.TenantID, batch.ID, claimed.Version, receipt, now)
	if err != nil || !inserted {
		t.Fatalf("record receipt = %v/%v", inserted, err)
	}
	items, err := repository.ListItems(context.Background(), batch.TenantID, batch.RequestID)
	if err != nil || len(items) != 1 {
		t.Fatalf("list items: %v (%d)", err, len(items))
	}
	if items[0].Status != application.HandoverItemTransferred || items[0].AppliedVersion != 8 {
		t.Fatalf("item not transferred: %+v", items[0])
	}

	replayed, err := repository.RecordReceipt(context.Background(), batch.TenantID, batch.ID, claimed.Version, receipt, now)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("the same idempotency key must not be recorded twice")
	}

	conflicting, err := repository.RecordReceipt(context.Background(), batch.TenantID, batch.ID, claimed.Version+5, application.HandoverTransferReceipt{
		System: "contract", ItemID: claimed.ID, IdempotencyKey: "handover:other", Status: application.HandoverReceiptApplied, AppliedVersion: 9,
	}, now)
	if err == nil || !errors.Is(err, application.ErrConflict) {
		t.Fatalf("stale version must conflict: inserted=%v err=%v", conflicting, err)
	}
}

func TestHandoverRetryableReceiptSchedulesNextAttemptOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	now := time.Now().UTC()
	batch := newHandoverBatch(t, repository, now)
	seedResponsibility(t, repository, batch, "contract-4", now)
	freezeHandoverBatch(t, repository, batch, now)
	claimed, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-1", now.Add(time.Minute), now)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v", ok, err)
	}
	if _, err := repository.RecordReceipt(context.Background(), batch.TenantID, batch.ID, claimed.Version, application.HandoverTransferReceipt{
		System: "contract", ItemID: claimed.ID, IdempotencyKey: claimed.IdempotencyKey,
		Status: application.HandoverReceiptRetryable, ReasonCode: "SUBSYSTEM_BUSY",
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-2", now.Add(time.Minute), now); err != nil || ok {
		t.Fatalf("claim before next_retry_at = %v/%v, want none", ok, err)
	}
	items, err := repository.ListItems(context.Background(), batch.TenantID, batch.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != application.HandoverItemPending || items[0].NextRetryAt == nil || items[0].LastErrorCode != "SUBSYSTEM_BUSY" {
		t.Fatalf("retryable receipt did not schedule a retry: %+v", items[0])
	}
	after := items[0].NextRetryAt.Add(time.Second)
	if _, ok, err := repository.ClaimNextItem(context.Background(), batch.TenantID, batch.ID, "lease-3", after.Add(time.Minute), after); err != nil || !ok {
		t.Fatalf("claim after the retry schedule = %v/%v", ok, err)
	}
}

func TestHandoverRetryableReceiptAdvancesToAppliedOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	ctx := context.Background()
	now := time.Now().UTC()
	batch := newHandoverBatch(t, repository, now)
	seedResponsibility(t, repository, batch, "contract-retry-applied", now)
	freezeHandoverBatch(t, repository, batch, now)
	first, ok, err := repository.ClaimNextItem(ctx, batch.TenantID, batch.ID, "lease-first", now.Add(time.Minute), now)
	if err != nil || !ok {
		t.Fatalf("first claim=%v err=%v", ok, err)
	}
	if _, err := repository.RecordReceipt(ctx, batch.TenantID, batch.ID, first.Version, application.HandoverTransferReceipt{System: "contract", ItemID: first.ID, IdempotencyKey: first.IdempotencyKey, Status: application.HandoverReceiptRetryable, ReasonCode: "SUBSYSTEM_BUSY"}, now); err != nil {
		t.Fatal(err)
	}
	items, err := repository.ListItems(ctx, batch.TenantID, batch.RequestID)
	if err != nil || len(items) != 1 || items[0].NextRetryAt == nil {
		t.Fatalf("retry checkpoint=%+v err=%v", items, err)
	}
	after := items[0].NextRetryAt.Add(time.Second)
	retry, ok, err := repository.ClaimNextItem(ctx, batch.TenantID, batch.ID, "lease-retry", after.Add(time.Minute), after)
	if err != nil || !ok || retry.IdempotencyKey != first.IdempotencyKey || retry.Attempts != 2 {
		t.Fatalf("retry claim=%+v ok=%v err=%v", retry, ok, err)
	}
	applied := application.HandoverTransferReceipt{System: "contract", ItemID: first.ID, IdempotencyKey: first.IdempotencyKey, Status: application.HandoverReceiptApplied, AppliedVersion: 8, EvidenceRef: "audit:retry-applied"}
	if _, err := repository.RecordReceipt(ctx, batch.TenantID, batch.ID, retry.Version, applied, after); err != nil {
		t.Fatal(err)
	}
	items, err = repository.ListItems(ctx, batch.TenantID, batch.RequestID)
	if err != nil || len(items) != 1 || items[0].Status != application.HandoverItemTransferred || items[0].AppliedVersion != 8 || items[0].LastErrorCode != "" || items[0].EvidenceRef != "audit:retry-applied" {
		t.Fatalf("applied item=%+v err=%v", items, err)
	}
	var receipts []struct {
		Status         string
		AppliedVersion uint64
		EvidenceRef    string
	}
	if err := database.Table("iam_personnel_handover_receipt").Where("tenant_id = ? AND batch_id = ? AND idempotency_key = ?", batch.TenantID, batch.ID, first.IdempotencyKey).Find(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Status != application.HandoverReceiptApplied || receipts[0].AppliedVersion != 8 || receipts[0].EvidenceRef != "audit:retry-applied" {
		t.Fatalf("same-key receipt did not advance: %+v", receipts)
	}
	if inserted, err := repository.RecordReceipt(ctx, batch.TenantID, batch.ID, retry.Version, applied, after); err != nil || inserted {
		t.Fatalf("terminal replay inserted=%v err=%v", inserted, err)
	}
}

func TestHandoverSystemStateAndBatchCompletionOnMySQL(t *testing.T) {
	database := handoverTestDatabase(t)
	repository := handoverRepository(t, database)
	now := time.Now().UTC()
	batch := newHandoverBatch(t, repository, now)

	if err := repository.UpsertSystemState(context.Background(), batch.TenantID, batch.ID, "contract", application.HandoverSystemApplicable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpsertSystemState(context.Background(), batch.TenantID, batch.ID, "portal", application.HandoverSystemNotInstalled, "", now); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkFrozen(context.Background(), batch.TenantID, batch.ID, "contract", now, 3); err != nil {
		t.Fatal(err)
	}
	states, err := repository.ListSystemStates(context.Background(), batch.TenantID, batch.ID)
	if err != nil || len(states) != 2 {
		t.Fatalf("system states: %v (%d)", err, len(states))
	}
	for _, state := range states {
		if state.System == "contract" && (!state.Frozen || state.Capability != application.HandoverSystemApplicable) {
			t.Fatalf("contract state = %+v", state)
		}
		if state.System == "portal" && state.Capability != application.HandoverSystemNotInstalled {
			t.Fatalf("portal state = %+v", state)
		}
	}
	if err := repository.CompleteBatch(context.Background(), batch.TenantID, batch.ID, application.HandoverBatchCompleted, now); err != nil {
		t.Fatal(err)
	}
	completed, err := repository.GetBatch(context.Background(), batch.TenantID, batch.RequestID)
	if err != nil || completed.Status != application.HandoverBatchCompleted || completed.CompletedAt == nil {
		t.Fatalf("batch not completed: %+v (%v)", completed, err)
	}
}
