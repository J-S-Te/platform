package infrastructure

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 审计导出下载契约变更的核心不变量：票据核销必须是数据库层的条件 UPDATE，并发兑换
// 同一票据时只有一个调用方能命中 used_at IS NULL AND expires_at > now。
//
// 这是真实 MySQL 测试，默认跳过；只在显式提供一次性、隔离数据库时执行：
//
//	PLATFORM_AUDIT_TICKET_TEST_DSN='user:pass@unix(/tmp/dsh-mysql.sock)/audit_ticket_test?parseTime=true&loc=UTC' go test -count=1 ./internal/platform/audit/infrastructure/ -run ExportDownloadTicket
//
// 测试只创建/使用 audit_export_download_ticket，并在结束后删除自己写入的租户行，不读写
// 其他业务表，不连接既有业务库。
func TestExportDownloadTicketConsumeIsAtomicOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_AUDIT_TICKET_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_AUDIT_TICKET_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open isolated MySQL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 只应用本表迁移：它是独立新增表，不依赖既有业务表，便于在空的一次性库上重复执行。
	body, err := migrations.Files.ReadFile("000137_audit_export_download_ticket.sql")
	if err != nil {
		t.Fatalf("read ticket migration: %v", err)
	}
	statements, err := migration.SplitStatements(string(body))
	if err != nil {
		t.Fatalf("parse ticket migration: %v", err)
	}
	for _, statement := range statements {
		if err := database.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatalf("apply ticket migration statement: %v", err)
		}
	}

	repository := &Repository{database: database}
	tenantID := fixedTestID("01JTICKETTENANT")
	otherTenantID := fixedTestID("01JTICKETOTHERT")
	jobID := fixedTestID("01JTICKETJOB")
	now := time.Now().UTC().Truncate(time.Millisecond)
	t.Cleanup(func() {
		if err := database.Where("tenant_id IN ?", []string{tenantID, otherTenantID}).Delete(&auditExportDownloadTicketModel{}).Error; err != nil {
			t.Errorf("cleanup ticket fixtures: %v", err)
		}
	})

	concurrentDigest := sha256.Sum256([]byte("concurrent-ticket"))
	if err := repository.CreateExportDownloadTicket(ctx, domain.ExportDownloadTicket{ID: fixedTestID("01JTICKETCONCUR"), TenantID: tenantID, JobID: jobID, TokenHash: concurrentDigest[:], ExpiresAt: now.Add(2 * time.Minute), CreatedAt: now}); err != nil {
		t.Fatalf("create concurrent ticket: %v", err)
	}

	const exchanges = 10
	states := make(chan domain.ExportDownloadTicketState, exchanges)
	errs := make(chan error, exchanges)
	var wait sync.WaitGroup
	for index := 0; index < exchanges; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			state, err := repository.ConsumeExportDownloadTicket(ctx, tenantID, jobID, concurrentDigest[:], time.Now().UTC())
			if err != nil {
				errs <- err
				return
			}
			states <- state
		}()
	}
	wait.Wait()
	close(states)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent consume error: %v", err)
	}
	consumed, invalid := 0, 0
	for state := range states {
		switch state {
		case domain.ExportDownloadTicketConsumed:
			consumed++
		case domain.ExportDownloadTicketInvalid:
			invalid++
		default:
			t.Fatalf("unexpected concurrent consume state %q", state)
		}
	}
	if consumed != 1 || invalid != exchanges-1 {
		t.Fatalf("concurrent consume: consumed=%d invalid=%d, want 1 and %d", consumed, invalid, exchanges-1)
	}

	// 跨任务/跨租户不能兑换：摘要相同也必须被范围条件拒绝。
	crossDigest := sha256.Sum256([]byte("cross-scope-ticket"))
	if err := repository.CreateExportDownloadTicket(ctx, domain.ExportDownloadTicket{ID: fixedTestID("01JTICKETCROSS0"), TenantID: tenantID, JobID: jobID, TokenHash: crossDigest[:], ExpiresAt: now.Add(2 * time.Minute), CreatedAt: now}); err != nil {
		t.Fatalf("create cross-scope ticket: %v", err)
	}
	for name, scope := range map[string]struct{ tenantID, jobID string }{
		"other job":    {tenantID: tenantID, jobID: fixedTestID("01JTICKETOTHERJ")},
		"other tenant": {tenantID: otherTenantID, jobID: jobID},
	} {
		state, err := repository.ConsumeExportDownloadTicket(ctx, scope.tenantID, scope.jobID, crossDigest[:], time.Now().UTC())
		if err != nil {
			t.Fatalf("%s consume: %v", name, err)
		}
		if state != domain.ExportDownloadTicketInvalid {
			t.Fatalf("%s consume state = %q, want INVALID", name, state)
		}
	}
	// 原范围仍然可用，证明上面的拒绝来自范围条件。
	if state, err := repository.ConsumeExportDownloadTicket(ctx, tenantID, jobID, crossDigest[:], time.Now().UTC()); err != nil || state != domain.ExportDownloadTicketConsumed {
		t.Fatalf("own scope consume = %q (err=%v), want CONSUMED", state, err)
	}

	// 过期票据：存在且未使用，但不能核销，必须回报 EXPIRED 以便 API 映射 410。
	expiredDigest := sha256.Sum256([]byte("expired-ticket"))
	if err := repository.CreateExportDownloadTicket(ctx, domain.ExportDownloadTicket{ID: fixedTestID("01JTICKETEXPIRY"), TenantID: tenantID, JobID: jobID, TokenHash: expiredDigest[:], ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("create expired ticket: %v", err)
	}
	if state, err := repository.ConsumeExportDownloadTicket(ctx, tenantID, jobID, expiredDigest[:], now); err != nil || state != domain.ExportDownloadTicketExpired {
		t.Fatalf("expired consume = %q (err=%v), want EXPIRED", state, err)
	}
	// 重复申请票据时顺带回收同一任务下的旧票据，避免票据表无界增长。
	if err := repository.CreateExportDownloadTicket(ctx, domain.ExportDownloadTicket{ID: fixedTestID("01JTICKETFRESH0"), TenantID: tenantID, JobID: jobID, TokenHash: sha256Sum("fresh-ticket"), ExpiresAt: now.Add(2 * time.Minute), CreatedAt: now}); err != nil {
		t.Fatalf("create fresh ticket: %v", err)
	}
	var staleRows int64
	if err := database.Model(&auditExportDownloadTicketModel{}).Where("id = ?", fixedTestID("01JTICKETEXPIRY")).Count(&staleRows).Error; err != nil {
		t.Fatalf("count stale tickets: %v", err)
	}
	if staleRows != 0 {
		t.Fatalf("stale ticket rows = %d, want 0 after issuing a new ticket", staleRows)
	}
}

// sha256Sum 返回 token 的 SHA-256 摘要，供测试构造票据哈希。
func sha256Sum(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

// fixedTestID 生成固定长度（CHAR(26)）的测试标识，避免夹具依赖全局 ID 生成器。
func fixedTestID(prefix string) string {
	const width = 26
	if len(prefix) >= width {
		return prefix[:width]
	}
	return prefix + strings.Repeat("0", width-len(prefix))
}
