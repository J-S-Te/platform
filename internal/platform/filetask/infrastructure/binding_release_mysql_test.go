package infrastructure_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
)

// 终态释放使用真实 MySQL 覆盖，仅接受显式启用的测试 DSN。共享
// 辅助函数只迁移和重置专用 file-gateway 测试 schema，绝不操作业务库。
func TestReleaseBindingFencesLateBindOnMySQL(t *testing.T) {
	db := cleanupTestDatabase(t)
	repo, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	past := now.Add(-48 * time.Hour)
	for _, id := range []string{"01CLEANUP0000000000000101", "01CLEANUP0000000000000102"} {
		seedCleanupFile(t, db, "tenant-release", cleanupFileSeed{ID: id, RetentionClass: "LONG_TERM", CreatedAt: past})
	}
	b := domain.FileBinding{ID: "01CLEANUP0000000000000201", TenantID: "tenant-release", ApplicationID: "app-cleanup", FileID: "01CLEANUP0000000000000101", ResourceType: "contract_template_source", ResourceID: "source-1", BindingType: "TEMPLATE_SOURCE", DisplayName: "source", CreatedBy: "app-cleanup", CreatedAt: now}
	if _, err := repo.CreateBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseBinding(context.Background(), b); err != nil {
		t.Fatalf("repeat release: %v", err)
	}
	if _, err := repo.CreateBinding(context.Background(), b); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("late bind err=%v", err)
	}
	var active int64
	if err := db.Table("file_binding").Where("file_id=? AND status='ACTIVE'", b.FileID).Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	// 同一文件仍可供不同来源代次使用。
	other := b
	other.ID = "01CLEANUP0000000000000202"
	other.ResourceID = "source-2"
	if _, err := repo.CreateBinding(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	before := b
	before.ID = "01CLEANUP0000000000000203"
	before.FileID = "01CLEANUP0000000000000102"
	if err := repo.ReleaseBinding(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateBinding(context.Background(), before); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("release-before-bind err=%v", err)
	}
	wrong := b
	wrong.ID = "01CLEANUP0000000000000204"
	wrong.ApplicationID = "other-app"
	if err := repo.ReleaseBinding(context.Background(), wrong); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("cross app release err=%v", err)
	}
	wrong = b
	wrong.ID = "01CLEANUP0000000000000205"
	wrong.TenantID = "other-tenant"
	if err := repo.ReleaseBinding(context.Background(), wrong); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("cross tenant release err=%v", err)
	}
	var retention, status string
	if err := db.Table("file_object").Select("retention_class,status").Where("id=?", b.FileID).Row().Scan(&retention, &status); err != nil || retention != "LONG_TERM" || status != "READY" {
		t.Fatalf("evidence retention changed: %s %s %v", retention, status, err)
	}
}

func TestReleaseBeforeV2UploadHasFileRowOnMySQL(t *testing.T) {
	db := cleanupTestDatabase(t)
	repo, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	b := domain.FileBinding{ID: "01CLEANUP0000000000000206", TenantID: "tenant-release", ApplicationID: "app-cleanup", FileID: "01CLEANUP0000000000000103", ResourceType: "CUSTOMER_IMPORT", ResourceID: "job-early", BindingType: "SOURCE", CreatedBy: "app-cleanup", CreatedAt: now}
	if err := db.Exec(`INSERT INTO file_upload_v2_session (id,tenant_id,application_id,application_code,authenticated_client_id,file_id,version_id,namespace,purpose,policy_version,original_name,declared_media_type,classification,expected_size,expected_sha256,resource_type,resource_id,binding_type,display_name,idempotency_key,upload_mode,status,created_at,updated_at) VALUES (?,?,?,'customer_and_opportunity','machine',?,'01CLEANUP0000000000000303','crm','customer-import','v2','source.xlsx','application/vnd.openxmlformats-officedocument.spreadsheetml.sheet','INTERNAL',1,UNHEX(REPEAT('ab',32)),?,?,?,'source','early-key','PROXY','CREATED',?,?)`, "01CLEANUP0000000000000403", b.TenantID, b.ApplicationID, b.FileID, b.ResourceType, b.ResourceID, b.BindingType, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	seedCleanupFile(t, db, b.TenantID, cleanupFileSeed{ID: b.FileID, RetentionClass: "TEMPORARY", Retention: &now, CreatedAt: now.Add(-48 * time.Hour)})
	if _, err := repo.CreateBinding(context.Background(), b); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("early release did not fence uploaded bytes: %v", err)
	}
}
