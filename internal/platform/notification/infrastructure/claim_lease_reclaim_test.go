package infrastructure

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 通知事件与失败投递在领取后都会先置为 PROCESSING + 租约。进程崩溃会把这些记录留在
// PROCESSING：旧领取条件只接受 ACCEPTED/RETRY（投递只接受 FAILED），于是它们永远不再
// 被处理，运营端"重试"入口也领不到。领取条件必须同时接受"租约已过期"的 PROCESSING。

func newNotificationDryRun(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return database
}

func TestClaimIngestionEventsReclaimsExpiredProcessingLease(t *testing.T) {
	database := newNotificationDryRun(t)
	now := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)

	// 与仓储中领取条件一致的两段式条件：待处理/待重试，或租约过期的 PROCESSING。
	claimable := "((status IN ? AND (next_retry_at IS NULL OR next_retry_at <= ?)) OR (status = ? AND locked_until IS NOT NULL AND locked_until < ?))"
	statement := database.Model(&ingestionModel{}).
		Where(claimable, []string{string(domain.IngestionStatusAccepted), string(domain.IngestionStatusRetry)}, now, string(domain.IngestionStatusProcessing), now).
		Order("received_at ASC,id ASC").Limit(10).Find(&[]ingestionModel{}).Statement
	sql := statement.SQL.String()

	if !strings.Contains(sql, "status = ?") || !strings.Contains(sql, "locked_until IS NOT NULL AND locked_until < ?") {
		t.Fatalf("通知事件领取条件缺少租约过期接管分支: %s", sql)
	}
	if !strings.Contains(sql, "next_retry_at IS NULL OR next_retry_at <= ?") {
		t.Fatalf("通知事件领取条件缺少到期重试分支: %s", sql)
	}
}

func TestClaimFailedDeliveriesReclaimsExpiredProcessingLease(t *testing.T) {
	database := newNotificationDryRun(t)
	now := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)

	claimable := "tenant_id = ? AND ((status = ? AND next_retry_at <= ?) OR (status = ? AND locked_until IS NOT NULL AND locked_until < ?))"
	statement := database.Model(&deliveryModel{}).
		Where(claimable, "tenant-1", domain.DeliveryStatusFailed, now, domain.DeliveryStatusProcessing, now).
		Order("next_retry_at ASC,id ASC").Limit(10).Find(&[]deliveryModel{}).Statement
	sql := statement.SQL.String()

	if !strings.Contains(sql, "locked_until IS NOT NULL AND locked_until < ?") {
		t.Fatalf("失败投递领取条件缺少租约过期接管分支: %s", sql)
	}
	if !strings.Contains(sql, "next_retry_at <= ?") {
		t.Fatalf("失败投递领取条件缺少到期重试分支: %s", sql)
	}
}

// 领取后的条件更新必须使用同一组条件，否则两个实例可能同时接管同一条记录。
func TestClaimConditionalUpdateReusesTheSameLeaseCondition(t *testing.T) {
	for _, path := range []string{"gorm_ingestion_repository.go", "gorm_repository.go"} {
		source := readRepositorySource(t, path)
		if !strings.Contains(source, "Where(\"id = ? AND \"+claimable") {
			t.Errorf("%s 的条件更新必须复用 claimable 条件（避免并发重复接管）", path)
		}
	}
}

func readRepositorySource(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
