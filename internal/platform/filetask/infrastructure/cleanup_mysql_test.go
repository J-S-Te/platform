package infrastructure_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filemigrations "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/migrations"
)

// cleanupTestDatabase 打开专用测试库并执行迁移；FILE_GATEWAY_TEST_DSN 未配置时跳过。
// 该 DSN 必须指向专用空数据库，不得复用业务库。
func cleanupTestDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("FILE_GATEWAY_TEST_DSN")
	if dsn == "" {
		t.Skip("FILE_GATEWAY_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := filemigrations.Run(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"file_binding_release", "file_upload_v2_session", "file_upload_session", "file_binding", "file_version", "file_object"} {
		if err := database.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return database
}

type cleanupFileSeed struct {
	ID             string
	RetentionClass string
	Retention      *time.Time
	CreatedAt      time.Time
	WithBinding    bool
}

func seedCleanupFile(t *testing.T, database *gorm.DB, tenantID string, seed cleanupFileSeed) {
	t.Helper()
	now := time.Now().UTC()
	// file_object.current_version_id / file_binding.id 均为 CHAR(26)，测试数据必须等长。
	versionID := seed.ID[:22] + "V" + seed.ID[23:]
	if err := database.Exec("INSERT INTO file_object (id, tenant_id, application_id, original_name, classification, status, version, current_version_no, current_version_id, retention_class, retention_until, created_at, updated_at) VALUES (?, ?, 'app-cleanup', 'demo.pdf', 'INTERNAL', 'READY', 1, 1, ?, ?, ?, ?, ?)",
		seed.ID, tenantID, versionID, seed.RetentionClass, seed.Retention, seed.CreatedAt.UTC(), now,
	).Error; err != nil {
		t.Fatalf("insert file_object %s: %v", seed.ID, err)
	}
	if err := database.Exec("INSERT INTO file_version (id, file_id, version_no, storage_relative_path, size_bytes, original_name, status, created_at) VALUES (?, ?, 1, ?, 12, 'demo.pdf', 'READY', ?)",
		versionID, seed.ID, "tenant/"+seed.ID+".pdf", seed.CreatedAt.UTC(),
	).Error; err != nil {
		t.Fatalf("insert file_version %s: %v", seed.ID, err)
	}
	if seed.WithBinding {
		if err := database.Exec("INSERT INTO file_binding (id, tenant_id, application_id, file_id, resource_type, resource_id, binding_type, display_name, status, sort_order, created_at) VALUES (?, ?, 'app-cleanup', ?, 'opportunity', 'opp-1', 'ATTACHMENT', 'demo.pdf', 'ACTIVE', 0, ?)",
			seed.ID[:24]+"B1", tenantID, seed.ID, now,
		).Error; err != nil {
			t.Fatalf("insert file_binding %s: %v", seed.ID, err)
		}
	}
}

// TestCleanupClaimHonoursRetentionClassAndWindowOnMySQL 验证通用清理只领取
// “TEMPORARY + 保留期已到 + 创建早于截止点 + 无活动绑定”的文件：
// 长期文件、未到期文件与绑定文件必须保持 READY（不被触碰）。
func TestCleanupClaimHonoursRetentionClassAndWindowOnMySQL(t *testing.T) {
	database := cleanupTestDatabase(t)
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	const tenantID = "tenant-cleanup-1"
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)
	future := now.Add(72 * time.Hour)

	seedCleanupFile(t, database, tenantID, cleanupFileSeed{ID: "01CLEANUP0000000000000001", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})
	seedCleanupFile(t, database, tenantID, cleanupFileSeed{ID: "01CLEANUP0000000000000002", RetentionClass: "LONG_TERM", Retention: &past, CreatedAt: past})
	seedCleanupFile(t, database, tenantID, cleanupFileSeed{ID: "01CLEANUP0000000000000003", RetentionClass: "TEMPORARY", Retention: &future, CreatedAt: past})
	seedCleanupFile(t, database, tenantID, cleanupFileSeed{ID: "01CLEANUP0000000000000004", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past, WithBinding: true})

	claimed, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected the eligible temporary file to be claimed")
	}
	if claimed.File.ID != "01CLEANUP0000000000000001" {
		t.Fatalf("claimed %s, want the eligible temporary file", claimed.File.ID)
	}
	for _, id := range []string{"01CLEANUP0000000000000002", "01CLEANUP0000000000000003", "01CLEANUP0000000000000004"} {
		var status string
		if err := database.Raw("SELECT status FROM file_object WHERE id = ?", id).Scan(&status).Error; err != nil {
			t.Fatal(err)
		}
		if status != "READY" {
			t.Fatalf("file %s status = %s, want READY (must not be touched)", id, status)
		}
	}
	if _, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantID, now, now); err != nil || ok {
		t.Fatalf("second claim = %v/%v, want no further candidate", ok, err)
	}
}

// TestCleanupWritesRequireClaimVersionOnMySQL 验证完成/释放操作携带领取版本条件：
// 版本被并发写入推进后必须返回冲突，不能覆盖他人的状态变更。
func TestCleanupWritesRequireClaimVersionOnMySQL(t *testing.T) {
	database := cleanupTestDatabase(t)
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	const tenantID = "tenant-cleanup-2"
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)
	seedCleanupFile(t, database, tenantID, cleanupFileSeed{ID: "01CLEANUP0000000000000011", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})

	claimed, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantID, now, now)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v", ok, err)
	}
	if claimed.File.Version == 0 {
		t.Fatal("claimed version must be the post-claim version")
	}
	wrong := claimed.File.Version + 5
	if err := repository.MarkDeleted(context.Background(), tenantID, claimed.File.ID, wrong, now); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("MarkDeleted with stale version err = %v, want ErrConflict", err)
	}
	if err := repository.ReleaseCleanupClaim(context.Background(), tenantID, claimed.File.ID, "READY", wrong, now); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("ReleaseCleanupClaim with stale version err = %v, want ErrConflict", err)
	}
	if err := repository.MarkDeleted(context.Background(), tenantID, claimed.File.ID, claimed.File.Version, now); err != nil {
		t.Fatalf("MarkDeleted with claim version: %v", err)
	}
}

// TestBindingAndCleanupDoNotInterleaveOnMySQL 验证清理领取与新增活动绑定共用同一文件行锁：
// 先绑定则清理不再领取；先领取进入 DELETING 后绑定必须被拒绝，避免误删。
func TestBindingAndCleanupDoNotInterleaveOnMySQL(t *testing.T) {
	database := cleanupTestDatabase(t)
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)

	const tenantBound = "tenant-cleanup-3"
	seedCleanupFile(t, database, tenantBound, cleanupFileSeed{ID: "01CLEANUP0000000000000021", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})
	if _, err := repository.CreateBinding(context.Background(), cleanupBinding(tenantBound, "01CLEANUP0000000000000021")); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if _, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantBound, now, now); err != nil || ok {
		t.Fatalf("claim after binding = %v/%v, want no claim", ok, err)
	}

	const tenantClaimed = "tenant-cleanup-4"
	seedCleanupFile(t, database, tenantClaimed, cleanupFileSeed{ID: "01CLEANUP0000000000000022", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})
	claimed, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantClaimed, now, now)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v", ok, err)
	}
	if _, err := repository.CreateBinding(context.Background(), cleanupBinding(tenantClaimed, claimed.File.ID)); err == nil {
		t.Fatal("binding a file that is already DELETING must be rejected")
	}
}

func cleanupBinding(tenantID, fileID string) domain.FileBinding {
	return domain.FileBinding{
		ID: fileID[:24] + "B1", TenantID: tenantID, ApplicationID: "app-cleanup",
		FileID: fileID, ResourceType: "opportunity", ResourceID: "opp-1", BindingType: "ATTACHMENT",
		DisplayName: "demo.pdf", CreatedAt: time.Now().UTC(),
	}
}

// TestListStaleDeletingIsBoundedAndTenantScopedOnMySQL 验证受控对账的输入集合：
// 只返回本租户、超过截止时间仍停留在 DELETING 的记录，且受 limit 约束；READY 记录不返回。
func TestListStaleDeletingIsBoundedAndTenantScopedOnMySQL(t *testing.T) {
	database := cleanupTestDatabase(t)
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)
	const tenantA = "tenant-reconcile-a"
	const tenantB = "tenant-reconcile-b"

	seedCleanupFile(t, database, tenantA, cleanupFileSeed{ID: "01RECONCILE0000000000000A", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})
	seedCleanupFile(t, database, tenantA, cleanupFileSeed{ID: "01RECONCILE0000000000000B", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})
	seedCleanupFile(t, database, tenantB, cleanupFileSeed{ID: "01RECONCILE0000000000000C", RetentionClass: "TEMPORARY", Retention: &past, CreatedAt: past})

	claimA, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantA, now, now)
	if err != nil || !ok {
		t.Fatalf("claim tenant A = %v/%v", ok, err)
	}
	if _, ok, err := repository.ClaimExpiredUnbound(context.Background(), tenantB, now, now); err != nil || !ok {
		t.Fatalf("claim tenant B = %v/%v", ok, err)
	}
	// 让两条 DELETING 记录看起来"停留已久"，另一条保持 READY。
	if err := database.Exec("UPDATE file_object SET updated_at = ? WHERE status = 'DELETING'", past).Error; err != nil {
		t.Fatal(err)
	}

	stale, err := repository.ListStaleDeleting(context.Background(), tenantA, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].File.ID != claimA.File.ID {
		t.Fatalf("stale = %+v, want only the claimed record of tenant A", stale)
	}
	if stale[0].File.Version != claimA.File.Version || stale[0].Version.StorageRelativePath == "" {
		t.Fatalf("stale record must carry claim version and storage path: %+v", stale[0])
	}
	if bounded, err := repository.ListStaleDeleting(context.Background(), tenantA, now, 1); err != nil || len(bounded) != 1 {
		t.Fatalf("limit=1 returned %d records (err=%v)", len(bounded), err)
	}
	if none, err := repository.ListStaleDeleting(context.Background(), tenantA, past.Add(-time.Hour), 10); err != nil || len(none) != 0 {
		t.Fatalf("records updated after the cutoff must not be returned: %d (err=%v)", len(none), err)
	}
}

