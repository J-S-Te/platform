package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 保留期清理：只删除仓储选出的过期 AUDIT_EXPORT 产物文件，并只把这些版本标记为
// DELETED；文件已缺失视为已清理，路径越界（../）的条目既不删除也不标记。
func TestRunOnceCleansExpiredExportArtifacts(t *testing.T) {
	storageRoot := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("tenant-1", "app-1", "2026", "01", "file-1", "version-1.csv"))
	absolutePath := filepath.Join(storageRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		t.Fatalf("create export dir: %v", err)
	}
	if err := os.WriteFile(absolutePath, []byte("event_id\n"), 0o600); err != nil {
		t.Fatalf("seed export file: %v", err)
	}
	// 存储根之外的诱饵文件：路径越界条目必须被拒绝，不能借此删除其他目录文件。
	outsidePath := filepath.Join(filepath.Dir(storageRoot), "outside.csv")
	if err := os.WriteFile(outsidePath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outsidePath) })

	repository := &exportRepositoryStub{artifacts: []domain.ExportArtifact{
		{FileVersionID: "version-1", StorageRelativePath: relative},
		{FileVersionID: "version-missing", StorageRelativePath: "tenant-1/app-1/2026/01/file-2/version-2.csv"},
		{FileVersionID: "version-escape", StorageRelativePath: "../outside.csv"},
	}}
	worker := newExportTestWorker(t, repository, storageRoot)
	worker.retention = time.Hour

	before := time.Now().UTC()
	if claimed := worker.RunOnce(context.Background()); claimed {
		t.Fatal("RunOnce claimed a job that was never queued")
	}
	if _, err := os.Stat(absolutePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired export file still exists (err=%v)", err)
	}
	if _, err := os.Stat(outsidePath); err != nil {
		t.Fatalf("cleanup deleted a file outside storage root: %v", err)
	}
	if want := []string{"version-1", "version-missing"}; !reflect.DeepEqual(repository.marked, want) {
		t.Fatalf("marked versions = %v, want %v", repository.marked, want)
	}
	if repository.markCallCount != 1 {
		t.Fatalf("MarkExportArtifactsDeleted calls = %d, want 1", repository.markCallCount)
	}
	if len(repository.listedBefore) != 1 || len(repository.listedLimits) != 1 {
		t.Fatalf("listedBefore=%v listedLimits=%v", repository.listedBefore, repository.listedLimits)
	}
	cutoff := repository.listedBefore[0]
	if want := before.Add(-time.Hour); cutoff.Before(want.Add(-time.Minute)) || cutoff.After(want.Add(time.Minute)) {
		t.Fatalf("cleanup cutoff = %v, want about %v", cutoff, want)
	}
	if repository.listedLimits[0] != application.ExportArtifactCleanupBatchSize {
		t.Fatalf("cleanup batch size = %d, want %d", repository.listedLimits[0], application.ExportArtifactCleanupBatchSize)
	}
}

// 清理阶段失败只告警：本轮仍要领取并完成导出任务。
func TestCleanupFailureDoesNotBlockExport(t *testing.T) {
	repository := &exportRepositoryStub{
		artifactErr: errors.New("audit_job read failed"),
		events:      sortedExportEvents(3),
		hasWork:     true,
		work:        domain.ExportWork{JobID: "job-1", TenantID: "tenant-1", ApplicationID: "app-1", Attempts: 1, MaxAttempts: 3, LockedBy: "worker-1"},
	}
	worker := newExportTestWorker(t, repository, t.TempDir())

	if claimed := worker.RunOnce(context.Background()); !claimed {
		t.Fatal("RunOnce did not process the queued job after cleanup failure")
	}
	if repository.completed == nil {
		t.Fatal("export job did not complete")
	}
	if repository.failedCode != "" {
		t.Fatalf("export job unexpectedly failed with %q", repository.failedCode)
	}
}

// 标记失败时文件已删除但版本保持可用，下一轮会重新选择；本轮不得 panic 或中断导出。
func TestCleanupMarkFailureKeepsVersionsForRetry(t *testing.T) {
	storageRoot := t.TempDir()
	relative := "tenant-1/app-1/2026/01/file-1/version-1.csv"
	absolutePath := filepath.Join(storageRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		t.Fatalf("create export dir: %v", err)
	}
	if err := os.WriteFile(absolutePath, []byte("csv"), 0o600); err != nil {
		t.Fatalf("seed export file: %v", err)
	}
	repository := &exportRepositoryStub{
		artifacts: []domain.ExportArtifact{{FileVersionID: "version-1", StorageRelativePath: relative}},
		markErr:   errors.New("file_version update failed"),
	}
	worker := newExportTestWorker(t, repository, storageRoot)

	if claimed := worker.RunOnce(context.Background()); claimed {
		t.Fatal("RunOnce claimed a job that was never queued")
	}
	if repository.markCallCount != 1 {
		t.Fatalf("MarkExportArtifactsDeleted calls = %d, want 1", repository.markCallCount)
	}
	if _, err := os.Stat(absolutePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired export file still exists (err=%v)", err)
	}
	if len(repository.marked) != 0 {
		t.Fatalf("failed mark must not record versions: %v", repository.marked)
	}
}

func TestExportRetentionPeriodIsThirtyDays(t *testing.T) {
	if ExportRetentionPeriod != 30*24*time.Hour {
		t.Fatalf("ExportRetentionPeriod = %v, want 30 days", ExportRetentionPeriod)
	}
	if (&ExportWorker{}).retentionPeriod() != ExportRetentionPeriod {
		t.Fatal("zero-value worker must fall back to the default retention period")
	}
}
