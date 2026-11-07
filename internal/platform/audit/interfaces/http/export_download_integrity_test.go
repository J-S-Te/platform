package http

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 下载必须能证明下发的字节就是审计证据：响应头同时给出 RFC 9530 Digest（sha-256 的
// base64）与 X-Checksum-Sha256（十六进制），两者都由 file_version.sha256 派生。
func TestDownloadExportReturnsEvidenceDigests(t *testing.T) {
	digest := sha256.Sum256([]byte(testExportContent))
	file := availableExportFile()
	file.SizeBytes = uint64(len(testExportContent))
	file.SHA256 = digest[:]
	handler, storageRoot, _ := newExportDownloadHandler(t, file)
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if got, want := response.Header().Get("X-Checksum-Sha256"), hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("X-Checksum-Sha256 = %q, want %q", got, want)
	}
	if got, want := response.Header().Get("Digest"), "sha-256="+base64.StdEncoding.EncodeToString(digest[:]); got != want {
		t.Fatalf("Digest = %q, want %q", got, want)
	}
	if response.Body.String() != testExportContent {
		t.Fatalf("body = %q, want exact export bytes", response.Body.String())
	}
	// 证据完整性增强不改变既有下载语义：附件名与媒体类型仍然保留。
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment", disposition)
	}
	if mediaType := response.Header().Get("Content-Type"); !strings.HasPrefix(mediaType, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", mediaType)
	}
}

// 落盘文件与 file_version.size_bytes 不一致说明产物被截断或篡改：必须在写出任何响应体
// 之前 fail-closed（500 + 不声明摘要），绝不下发半截证据文件。
func TestDownloadExportRejectsSizeMismatch(t *testing.T) {
	digest := sha256.Sum256([]byte(testExportContent))
	file := availableExportFile()
	// 记录的大小比实际落盘内容多 10 字节，模拟截断。
	file.SizeBytes = uint64(len(testExportContent) + 10)
	file.SHA256 = digest[:]
	handler, storageRoot, _ := newExportDownloadHandler(t, file)
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for size mismatch; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "evt-1") {
		t.Fatalf("size mismatch must not return file bytes; body=%s", response.Body.String())
	}
	if digestHeader := response.Header().Get("Digest"); digestHeader != "" {
		t.Fatalf("Digest = %q, want no digest for a rejected artifact", digestHeader)
	}
	if checksum := response.Header().Get("X-Checksum-Sha256"); checksum != "" {
		t.Fatalf("X-Checksum-Sha256 = %q, want no checksum for a rejected artifact", checksum)
	}
}

// 库内缺少 sha256 证据时同样不能假装完整：没有可验证摘要就拒绝下发。
func TestDownloadExportRejectsMissingDigest(t *testing.T) {
	file := availableExportFile()
	file.SizeBytes = uint64(len(testExportContent))
	file.SHA256 = nil // file_version.sha256 缺失（证据元数据不完整）
	handler, storageRoot, _ := newExportDownloadHandler(t, file)
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 without digest metadata; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "evt-1") {
		t.Fatalf("missing digest must not return file bytes; body=%s", response.Body.String())
	}
}
