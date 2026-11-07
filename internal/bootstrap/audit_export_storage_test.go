package bootstrap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// D6 独立导出目录：导出 worker（写/清理）与下载 handler（读取）必须从同一个装配点取根，
// 两边根分叉会让 worker 刚写出的产物在下载端直接变成 404。
func TestAuditExportStorageRootSharedByExportWorkerAndHandler(t *testing.T) {
	fileStorageRoot := filepath.Join(t.TempDir(), "uploads")
	auditExportRoot := filepath.Join(fileStorageRoot, "audit-exports")
	cfg := config.Config{
		FileStorageRoot:        fileStorageRoot,
		AuditExportStorageRoot: auditExportRoot,
		Worker:                 config.WorkerConfig{ID: "worker-1", PollInterval: time.Second, StaleLockTimeout: time.Minute},
	}
	service := testAuditExportService(t)

	worker, err := newAuditExportWorker(cfg, service, testLogger(), 24*time.Hour)
	if err != nil {
		t.Fatalf("new audit export worker: %v", err)
	}
	handler, err := newAuditExportHandler(cfg, service, testLogger())
	if err != nil {
		t.Fatalf("new audit export handler: %v", err)
	}

	if worker.StorageRoot() != handler.StorageRoot() {
		t.Fatalf("export worker root = %q, handler root = %q, want the same AUDIT_EXPORT_STORAGE_ROOT",
			worker.StorageRoot(), handler.StorageRoot())
	}
	if handler.StorageRoot() != auditExportRoot {
		t.Fatalf("handler root = %q, want %q", handler.StorageRoot(), auditExportRoot)
	}
	// 兼容回退只读根仍是平台上传根：worker 永远不写它，handler 只在新根缺失时读它。
	if handler.LegacyStorageRoot() != fileStorageRoot {
		t.Fatalf("handler legacy root = %q, want FILE_STORAGE_ROOT %q", handler.LegacyStorageRoot(), fileStorageRoot)
	}
}

// 导出根与上传根共用时必须装配失败：否则保留期清理会直接删除业务上传文件。
func TestAuditExportStorageRejectsSharedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "uploads")
	cfg := config.Config{FileStorageRoot: root, AuditExportStorageRoot: root}

	if _, err := auditExportStorageFrom(cfg); err == nil {
		t.Fatal("auditExportStorageFrom() error = nil, want shared root rejected")
	} else if !strings.Contains(err.Error(), "AUDIT_EXPORT_STORAGE_ROOT") {
		t.Fatalf("auditExportStorageFrom() error = %v, want mention AUDIT_EXPORT_STORAGE_ROOT", err)
	}
}

// 未经过 Load 的 Config（缺 AuditExportStorageRoot）必须装配失败，而不是悄悄写回上传根。
func TestAuditExportStorageRequiresDedicatedRoot(t *testing.T) {
	cfg := config.Config{FileStorageRoot: filepath.Join(t.TempDir(), "uploads")}

	if _, err := auditExportStorageFrom(cfg); err == nil {
		t.Fatal("auditExportStorageFrom() error = nil, want missing export root rejected")
	} else if !strings.Contains(err.Error(), "AUDIT_EXPORT_STORAGE_ROOT") {
		t.Fatalf("auditExportStorageFrom() error = %v, want mention AUDIT_EXPORT_STORAGE_ROOT", err)
	}
}

// testAuditExportService 构造只用于装配断言的最小审计服务；仓储方法不会被调用。
func testAuditExportService(t *testing.T) *auditapplication.Service {
	t.Helper()
	service, err := auditapplication.NewService(bootstrapAuditRepositoryStub{}, bootstrapTestIDs{}, bootstrapTestClock{})
	if err != nil {
		t.Fatalf("new audit service: %v", err)
	}
	return service
}

type bootstrapTestIDs struct{}

func (bootstrapTestIDs) New(time.Time) (string, error) { return "01J00000000000000000000BSP", nil }

type bootstrapTestClock struct{}

func (bootstrapTestClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// bootstrapAuditRepositoryStub 只满足 application.Repository 接口，所有方法都失败关闭：
// 装配测试不触碰数据库或存储。编译器保证接口随实现同步扩展。
type bootstrapAuditRepositoryStub struct{}

func (bootstrapAuditRepositoryStub) Ingest(context.Context, string, auditapplication.EventInput, time.Time) (auditdomain.Receipt, error) {
	return auditdomain.Receipt{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) IngestBatch(context.Context, string, auditapplication.BatchDeliveryInput, []auditapplication.EventInput, time.Time) ([]auditdomain.Receipt, error) {
	return nil, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) List(context.Context, string, auditapplication.PageRequest) (auditapplication.PageResult[auditdomain.Event], error) {
	return auditapplication.PageResult[auditdomain.Event]{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) Get(context.Context, string, string) (auditdomain.Event, error) {
	return auditdomain.Event{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) IngestionHealth(context.Context, string, time.Time) (auditapplication.IngestionHealth, error) {
	return auditapplication.IngestionHealth{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) CreateExportJob(context.Context, string, string, auditapplication.PageRequest, string, time.Time) (auditdomain.ExportJob, error) {
	return auditdomain.ExportJob{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) GetExportJob(context.Context, string, string) (auditdomain.ExportJob, error) {
	return auditdomain.ExportJob{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) ClaimExportJob(context.Context, string, time.Time, time.Time) (auditdomain.ExportWork, bool, error) {
	return auditdomain.ExportWork{}, false, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) ListExportEvents(context.Context, string, auditdomain.ExportQuery, auditdomain.ExportCursor, int) (auditdomain.ExportEventPage, error) {
	return auditdomain.ExportEventPage{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) CompleteExportJob(context.Context, auditdomain.ExportWork, auditdomain.ExportFile, time.Time) error {
	return auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) FailExportJob(context.Context, auditdomain.ExportWork, string, string, time.Time, time.Time) error {
	return auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) GetExportFile(context.Context, string, string) (auditdomain.ExportFile, error) {
	return auditdomain.ExportFile{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) ListExpiredExportArtifacts(context.Context, time.Time, int) ([]auditdomain.ExportArtifact, error) {
	return nil, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) MarkExportArtifactsDeleted(context.Context, []string) error {
	return auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) VerifyPayloadHash(context.Context, string, string, []byte) (auditdomain.PayloadHashVerification, error) {
	return auditdomain.PayloadHashVerification{}, auditapplication.ErrNotFound
}
func (bootstrapAuditRepositoryStub) VerifyEventChain(context.Context, string, string, string, uint64, int) (auditdomain.ChainVerification, error) {
	return auditdomain.ChainVerification{}, auditapplication.ErrNotFound
}
