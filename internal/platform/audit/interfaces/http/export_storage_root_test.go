package http

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// D6 独立导出目录：下载读取必须优先使用 AUDIT_EXPORT_STORAGE_ROOT 对应的新根，
// 只在新根缺失该相对路径时只读回退旧的 FILE_STORAGE_ROOT，并留下 WARN 迁移证据。
func TestDownloadExportPrefersAuditExportStorageRoot(t *testing.T) {
	primaryRoot, legacyRoot := t.TempDir(), t.TempDir()
	handler, _, logs := newExportStorageHandler(t, availableExportFile(), primaryRoot, legacyRoot)
	// 同一相对路径在两个根下都存在且内容不同：只能返回新根的那份，证明优先级。
	seedExportFile(t, primaryRoot, testExportRelativePath, testExportContent)
	seedExportFile(t, legacyRoot, testExportRelativePath, "event_id,result\nevt-legacy,SUCCESS\n")
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != testExportContent {
		t.Fatalf("body = %q, want artifact from the audit export root", response.Body.String())
	}
	if strings.Contains(logs.String(), "legacy") {
		t.Fatalf("new-root hit must not log a legacy fallback warning; logs=%s", logs.String())
	}
}

// 兼容窗口：上一 release 的产物仍躺在 FileStorageRoot 下。新根缺失时只读回退必须可下载，
// 且必须打 WARN 让运维知道仍有产物未迁移。
func TestDownloadExportFallsBackToLegacyFileStorageRootWithWarning(t *testing.T) {
	primaryRoot, legacyRoot := t.TempDir(), t.TempDir()
	handler, _, logs := newExportStorageHandler(t, availableExportFile(), primaryRoot, legacyRoot)
	seedExportFile(t, legacyRoot, testExportRelativePath, testExportContent)
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from legacy read-only fallback; body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != testExportContent {
		t.Fatalf("body = %q, want legacy artifact bytes", response.Body.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "legacy") {
		t.Fatalf("logs = %s, want a WARN about the legacy storage-root fallback", logs.String())
	}
}

// 两个根都没有该产物时仍然是 404，而不是回退到任意路径或 500。
func TestDownloadExportReportsNotFoundWhenNeitherRootHoldsArtifact(t *testing.T) {
	primaryRoot, legacyRoot := t.TempDir(), t.TempDir()
	handler, repository, _ := newExportStorageHandler(t, availableExportFile(), primaryRoot, legacyRoot)
	repository.addTicket("tenant-1", "job-1", "ticket-for-missing-artifact", testTicketExpiry())

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", "ticket-for-missing-artifact"))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the artifact is missing from both roots; body=%s", response.Code, response.Body.String())
	}
}

// Job API 证据字段：任务完成后响应必须带 file_version_id / size_bytes / sha256(hex)，
// 调用方因此可以在下载前核对证据标识与摘要，而不是盲目信任 download_url。
func TestGetExportJobExposesExportEvidenceWhenCompleted(t *testing.T) {
	file := availableExportFile()
	file.VersionID = "version-1"
	handler, repository, _ := newExportStorageHandler(t, file, t.TempDir(), t.TempDir())
	repository.job = domain.ExportJob{JobID: "job-1", Status: "SUCCEEDED", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	response := httptest.NewRecorder()
	handler.GetExportJob(response, exportJobRequest("job-1"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			FileVersionID string `json:"file_version_id"`
			SizeBytes     uint64 `json:"size_bytes"`
			SHA256        string `json:"sha256"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode export job response: %v; body=%s", err, response.Body.String())
	}
	if got, want := envelope.Data.FileVersionID, "version-1"; got != want {
		t.Fatalf("file_version_id = %q, want %q", got, want)
	}
	if got, want := envelope.Data.SizeBytes, file.SizeBytes; got != want {
		t.Fatalf("size_bytes = %d, want %d", got, want)
	}
	if got, want := envelope.Data.SHA256, hex.EncodeToString(file.SHA256); got != want {
		t.Fatalf("sha256 = %q, want hex %q", got, want)
	}
	if decoded, err := hex.DecodeString(envelope.Data.SHA256); err != nil || len(decoded) != exportSHA256Length {
		t.Fatalf("sha256 = %q, want 32 hex-encoded bytes (err=%v)", envelope.Data.SHA256, err)
	}
}

// 进行中任务没有产物：证据字段缺省为空，查询本身不能因此报错。
func TestGetExportJobOmitsEvidenceBeforeCompletion(t *testing.T) {
	handler, repository, _ := newExportStorageHandler(t, availableExportFile(), t.TempDir(), t.TempDir())
	repository.job = domain.ExportJob{JobID: "job-1", Status: "RUNNING", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	response := httptest.NewRecorder()
	handler.GetExportJob(response, exportJobRequest("job-1"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode export job response: %v; body=%s", err, response.Body.String())
	}
	for _, field := range []string{"file_version_id", "size_bytes", "sha256"} {
		if _, found := envelope.Data[field]; found {
			t.Fatalf("running job response unexpectedly exposes %q: %s", field, response.Body.String())
		}
	}
}

// 产物已被保留期清理（SUCCEEDED 但 file_version 已删除）时，证据缺省为空且仍返回任务状态。
func TestGetExportJobToleratesCleanedArtifact(t *testing.T) {
	handler, repository, _ := newExportStorageHandler(t, availableExportFile(), t.TempDir(), t.TempDir())
	repository.job = domain.ExportJob{JobID: "job-1", Status: "SUCCEEDED", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	repository.fileErr = application.ErrExpired

	response := httptest.NewRecorder()
	handler.GetExportJob(response, exportJobRequest("job-1"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a cleaned artifact; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "file_version_id") {
		t.Fatalf("cleaned artifact response must omit evidence: %s", response.Body.String())
	}
}

// newExportStorageHandler 用显式的新/旧存储根构造下载 handler，并把 WARN 及以上日志
// 写入返回的缓冲区，用于断言兼容回退的告警确实出现。
func newExportStorageHandler(t *testing.T, file domain.ExportFile, auditExportRoot, fileStorageRoot string) (*Handler, *exportDownloadRepositoryStub, *bytes.Buffer) {
	t.Helper()
	repository := &exportDownloadRepositoryStub{file: file}
	service, err := application.NewService(repository, exportDownloadIDs{}, exportDownloadClock{})
	if err != nil {
		t.Fatalf("new audit service: %v", err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	handler, err := NewHandler(service, logger, filepath.Clean(auditExportRoot), filepath.Clean(fileStorageRoot))
	if err != nil {
		t.Fatalf("new audit handler: %v", err)
	}
	return handler, repository, logs
}

// exportJobRequest 构造导出任务查询请求；jobID 通过路由参数传入。
func exportJobRequest(jobID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/export-jobs/"+jobID, nil)
	request.SetPathValue("job_id", jobID)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"}}))
}
