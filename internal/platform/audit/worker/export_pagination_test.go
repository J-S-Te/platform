package worker

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 审计 P2：导出不再对总量设失败阈值。10001 条事件必须按稳定顺序完整写出，
// 跨批不重复、不遗漏；文件哈希覆盖最终字节。
func TestWriteCSVExportsMoreThanTenThousandEventsAcrossBatches(t *testing.T) {
	const total = 10001
	repository := &exportRepositoryStub{events: sortedExportEvents(total)}
	worker := newExportTestWorker(t, repository, t.TempDir())
	worker.exportBatchSize = 1000

	file, absolutePath, count, err := worker.writeCSV(context.Background(), domain.ExportWork{TenantID: "tenant-1", ApplicationID: "app-1", JobID: "job-large"})
	if err != nil {
		t.Fatalf("writeCSV error = %v", err)
	}
	if count != total {
		t.Fatalf("written event count = %d, want %d", count, total)
	}
	if file.SizeBytes == 0 || len(file.SHA256) != sha256.Size || file.OriginalName != "audit-export-job-large.csv" {
		t.Fatalf("export file metadata = %+v", file)
	}

	raw, err := os.ReadFile(absolutePath)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	digest := sha256.Sum256(raw)
	if string(digest[:]) != string(file.SHA256) {
		t.Fatal("export SHA-256 does not cover the published file bytes")
	}
	if !strings.HasPrefix(string(raw), "\xef\xbb\xbf") {
		t.Fatal("export CSV missing UTF-8 BOM")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatalf("parse export csv: %v", err)
	}
	if len(records) != total+1 {
		t.Fatalf("rows = %d, want header + %d data rows", len(records), total)
	}
	// 期望顺序：id 从 total 递减到 1，且每 3 条共享 occurred_at（游标边界）。
	for index := 1; index < len(records); index++ {
		wantID := "evt-" + strconv.Itoa(total-(index-1))
		if got := records[index][0]; got != wantID {
			t.Fatalf("row %d event_id = %q, want %q", index, got, wantID)
		}
	}
	// 分页读取次数与批量一致：10 个整批 + 1 个尾批。
	if len(repository.listCalls) != 11 {
		t.Fatalf("ListExportEvents calls = %d, want 11", len(repository.listCalls))
	}
	for index, limit := range repository.listLimits {
		if limit != 1000 {
			t.Fatalf("batch %d limit = %d, want 1000", index+1, limit)
		}
	}
	if !repository.listCalls[0].OccurredAt.IsZero() || repository.listCalls[0].ID != 0 {
		t.Fatalf("first batch cursor = %+v, want zero cursor", repository.listCalls[0])
	}
	for index := 1; index < len(repository.listCalls); index++ {
		if repository.listCalls[index].ID == 0 {
			t.Fatalf("batch %d received an empty cursor", index+1)
		}
	}
	for index := 2; index < len(repository.listCalls); index++ {
		if !cursorAdvanced(repository.listCalls[index-1], repository.listCalls[index]) {
			t.Fatalf("cursor did not advance between batch %d and %d: %+v -> %+v", index, index+1, repository.listCalls[index-1], repository.listCalls[index])
		}
	}
}

func TestWriteCSVEmptyExportWritesHeaderOnly(t *testing.T) {
	worker := newExportTestWorker(t, &exportRepositoryStub{}, t.TempDir())
	file, absolutePath, count, err := worker.writeCSV(context.Background(), domain.ExportWork{TenantID: "tenant-1", ApplicationID: "app-1", JobID: "job-empty"})
	if err != nil {
		t.Fatalf("writeCSV error = %v", err)
	}
	if count != 0 || file.SizeBytes == 0 {
		t.Fatalf("count=%d metadata=%+v", count, file)
	}
	raw, err := os.ReadFile(absolutePath)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatalf("parse export csv: %v", err)
	}
	if len(records) != 1 || len(records[0]) != len(exportHeader) {
		t.Fatalf("empty export rows = %d, columns = %d; want header only", len(records), len(records[0]))
	}
}

// 单批读取失败时任务必须失败、不发布文件，并删除临时文件与目录内残留。
func TestBatchFailureFailsJobAndRemovesTemporaryFile(t *testing.T) {
	storageRoot := t.TempDir()
	repository := &exportRepositoryStub{
		events:     sortedExportEvents(25),
		listErr:    errors.New("audit_event read failed"),
		failAtCall: 3,
		hasWork:    true,
		work:       domain.ExportWork{JobID: "job-fail", TenantID: "tenant-1", ApplicationID: "app-1", Attempts: 1, MaxAttempts: 3, LockedBy: "worker-1"},
	}
	worker := newExportTestWorker(t, repository, storageRoot)
	worker.exportBatchSize = 10

	if claimed := worker.ProcessOne(context.Background()); !claimed {
		t.Fatal("ProcessOne did not claim the queued job")
	}
	if repository.completed != nil {
		t.Fatal("failed export must not publish file metadata")
	}
	if repository.failedCode != "AUDIT_EXPORT_FAILED" {
		t.Fatalf("failure code = %q, want AUDIT_EXPORT_FAILED", repository.failedCode)
	}
	if strings.Contains(repository.failedMessage, "more than 10000") {
		t.Fatalf("failure message still describes the removed size limit: %q", repository.failedMessage)
	}
	// 不留下部分文件：目录内不得有 CSV 或 .tmp。
	var leftovers []string
	_ = filepath.Walk(storageRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("failed export left files behind: %v", leftovers)
	}
}

func cursorAdvanced(previous, next domain.ExportCursor) bool {
	if next.OccurredAt.Before(previous.OccurredAt) {
		return true
	}
	return next.OccurredAt.Equal(previous.OccurredAt) && next.ID < previous.ID
}
