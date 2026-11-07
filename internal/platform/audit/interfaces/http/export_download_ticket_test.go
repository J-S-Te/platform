package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 审计导出下载契约变更：GET .../download 不再凭会话直接返回文件，必须先申请短时一次性
// 票据，再用票据兑换；票据一次性、原子核销，并发兑换最多一个成功。
const (
	testExportRelativePath = "tenant-1/app-1/2026/01/file-1/version-1.csv"
	testExportContent      = "event_id,result\nevt-1,SUCCESS\n"
)

func availableExportFile() domain.ExportFile {
	// 夹具必须携带真实证据元数据：下载端会校验落盘大小与 sha256，缺失即拒绝下发。
	digest := sha256.Sum256([]byte(testExportContent))
	return domain.ExportFile{
		FileID:              "file-1",
		StorageRelativePath: testExportRelativePath,
		OriginalName:        "审计导出.csv",
		MediaType:           "text/csv; charset=utf-8",
		Status:              "ACTIVE",
		SizeBytes:           uint64(len(testExportContent)),
		SHA256:              digest[:],
	}
}

type issuedTicket struct {
	Ticket      string    `json:"ticket"`
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// issueTicket 通过签发接口取票据，并要求签发成功；失败时终止当前用例。
func issueTicket(t *testing.T, handler *Handler, jobID string) issuedTicket {
	t.Helper()
	response := httptest.NewRecorder()
	handler.CreateExportDownloadTicket(response, exportDownloadTicketRequest(jobID))
	if response.Code != http.StatusCreated {
		t.Fatalf("issue ticket status = %d, want 201; body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data issuedTicket `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode ticket response: %v; body=%s", err, response.Body.String())
	}
	return envelope.Data
}

func TestCreateExportDownloadTicketReturnsSingleUseBearerAndStoresOnlyDigest(t *testing.T) {
	handler, storageRoot, repository := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)

	issued := issueTicket(t, handler, "job-1")

	// 32 字节随机 token 以 base64url（无填充）编码：足够长，且不可预测。
	decoded, err := base64.RawURLEncoding.DecodeString(issued.Ticket)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("ticket = %q, want base64url of 32 random bytes (err=%v)", issued.Ticket, err)
	}
	if issued.DownloadURL != "/api/v1/audit/export-jobs/job-1/download" {
		t.Fatalf("download_url = %q", issued.DownloadURL)
	}
	if want := (exportDownloadClock{}).Now().Add(application.ExportDownloadTicketTTL); !issued.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", issued.ExpiresAt, want)
	}
	// 服务端只保存摘要：库里既没有明文 token，也不能用库里的值反推出 token。
	hashes := repository.heldTokenHashes()
	if len(hashes) != 1 {
		t.Fatalf("stored ticket hashes = %d, want 1", len(hashes))
	}
	digest := sha256.Sum256([]byte(issued.Ticket))
	if !bytes.Equal(hashes[0], digest[:]) {
		t.Fatalf("stored token hash does not match sha256(ticket)")
	}
	if string(hashes[0]) == issued.Ticket {
		t.Fatalf("plaintext ticket must never be persisted")
	}
}

func TestCreateExportDownloadTicketRefusesUnavailableArtifacts(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		file    domain.ExportFile
		fileErr error
		seed    bool
		status  int
		code    string
	}{
		// 任务不属于当前租户或从未完成：既有查询按租户 + SUCCEEDED 过滤，统一 404。
		{name: "unknown job", fileErr: application.ErrNotFound, status: http.StatusNotFound, code: "PLATFORM_NOT_FOUND"},
		// 产物被保留期清理（file_version 标记 DELETED）：410 提示重新发起导出。
		{name: "cleaned artifact", file: domain.ExportFile{FileID: "file-1", StorageRelativePath: testExportRelativePath, OriginalName: "x.csv", MediaType: "text/csv", Status: "DELETED"}, seed: true, status: http.StatusGone, code: "PLATFORM_GONE"},
		{name: "removed artifact", file: domain.ExportFile{FileID: "file-1", StorageRelativePath: testExportRelativePath, OriginalName: "x.csv", MediaType: "text/csv", Status: "REMOVED"}, seed: true, status: http.StatusGone, code: "PLATFORM_GONE"},
		// 任务标记完成但磁盘文件已丢失：不签发注定无法兑换的票据。
		{name: "missing file", file: availableExportFile(), status: http.StatusNotFound, code: "PLATFORM_NOT_FOUND"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			handler, storageRoot, repository := newExportDownloadHandler(t, fixture.file)
			repository.fileErr = fixture.fileErr
			if fixture.seed {
				seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
			}
			response := httptest.NewRecorder()
			handler.CreateExportDownloadTicket(response, exportDownloadTicketRequest("job-1"))

			if response.Code != fixture.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, fixture.status, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), fixture.code) {
				t.Fatalf("body = %s, want error code %s", response.Body.String(), fixture.code)
			}
			if hashes := repository.heldTokenHashes(); len(hashes) != 0 {
				t.Fatalf("refused issuance must not persist a ticket, found %d", len(hashes))
			}
		})
	}
}

func TestDownloadExportRequiresTicket(t *testing.T) {
	handler, storageRoot, repository := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", ""))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without ticket; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "PLATFORM_AUDIT_EXPORT_TICKET_REQUIRED") {
		t.Fatalf("body = %s, want ticket required error code", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "evt-1") {
		t.Fatalf("download without ticket must not return file bytes")
	}
	if hashes := repository.heldTokenHashes(); len(hashes) != 0 {
		t.Fatalf("download without ticket must not consume anything")
	}

	// 猜错/伪造的票据与已用票据同样按 403 拒绝，不区分“不存在”和“已兑换”。
	response = httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", "forged-ticket"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for forged ticket; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "PLATFORM_AUDIT_EXPORT_TICKET_INVALID") {
		t.Fatalf("body = %s, want invalid ticket error code", response.Body.String())
	}
}

func TestDownloadExportServesBytesOnceAndRejectsReuse(t *testing.T) {
	handler, storageRoot, _ := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	issued := issueTicket(t, handler, "job-1")

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != testExportContent {
		t.Fatalf("body = %q, want exact export bytes", response.Body.String())
	}
	if mediaType := response.Header().Get("Content-Type"); !strings.HasPrefix(mediaType, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", mediaType)
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment", disposition)
	}

	// 同一票据第二次兑换必须失败，且不再输出文件字节。
	reused := httptest.NewRecorder()
	handler.DownloadExport(reused, exportDownloadRequest("job-1", issued.Ticket))
	if reused.Code != http.StatusForbidden {
		t.Fatalf("reuse status = %d, want 403; body=%s", reused.Code, reused.Body.String())
	}
	if !strings.Contains(reused.Body.String(), "PLATFORM_AUDIT_EXPORT_TICKET_INVALID") {
		t.Fatalf("body = %s, want invalid ticket error code", reused.Body.String())
	}
	if strings.Contains(reused.Body.String(), "evt-1") {
		t.Fatalf("reused ticket must not return file bytes")
	}
}

func TestDownloadExportRejectsExpiredTicket(t *testing.T) {
	handler, storageRoot, repository := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	expiredAt := exportDownloadClock{}.Now().Add(-time.Second)
	repository.addTicket("tenant-1", "job-1", "expired-ticket", expiredAt)

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", "expired-ticket"))
	if response.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "PLATFORM_AUDIT_EXPORT_TICKET_EXPIRED") {
		t.Fatalf("body = %s, want expired ticket error code", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "evt-1") {
		t.Fatalf("expired ticket must not return file bytes")
	}
}

func TestDownloadExportRejectsTicketForAnotherJobOrTenant(t *testing.T) {
	handler, storageRoot, repository := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)
	repository.addTicket("tenant-1", "job-other", "ticket-for-other-job", testTicketExpiry())
	repository.addTicket("tenant-2", "job-1", "ticket-for-other-tenant", testTicketExpiry())

	for name, request := range map[string]*http.Request{
		"other job":    exportDownloadRequest("job-1", "ticket-for-other-job"),
		"other tenant": exportDownloadRequest("job-1", "ticket-for-other-tenant"),
	} {
		response := httptest.NewRecorder()
		handler.DownloadExport(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403; body=%s", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "evt-1") {
			t.Fatalf("%s: cross-scope ticket must not return file bytes", name)
		}
	}

	// 同一张票据在其所属任务上仍可正常兑换，证明上述拒绝来自范围校验而不是误伤。
	issued := issueTicket(t, handler, "job-1")
	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", issued.Ticket))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for own ticket; body=%s", response.Code, response.Body.String())
	}
}

func TestDownloadExportAllowsOnlyOneConcurrentExchange(t *testing.T) {
	handler, storageRoot, _ := newExportDownloadHandler(t, availableExportFile())
	seedExportFile(t, storageRoot, testExportRelativePath, testExportContent)

	const concurrentExchanges = 5
	issued := issueTicket(t, handler, "job-1")
	responses := make([]*httptest.ResponseRecorder, concurrentExchanges)
	var wait sync.WaitGroup
	for index := 0; index < concurrentExchanges; index++ {
		responses[index] = httptest.NewRecorder()
		wait.Add(1)
		go func(recorder *httptest.ResponseRecorder) {
			defer wait.Done()
			handler.DownloadExport(recorder, exportDownloadRequest("job-1", issued.Ticket))
		}(responses[index])
	}
	wait.Wait()

	succeeded := 0
	for _, response := range responses {
		switch response.Code {
		case http.StatusOK:
			succeeded++
			if response.Body.String() != testExportContent {
				t.Fatalf("successful exchange body = %q, want exact export bytes", response.Body.String())
			}
		case http.StatusForbidden:
			if strings.Contains(response.Body.String(), "evt-1") {
				t.Fatalf("rejected concurrent exchange must not return file bytes")
			}
		default:
			t.Fatalf("unexpected status %d for concurrent exchange; body=%s", response.Code, response.Body.String())
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent exchanges = %d, want exactly 1", succeeded)
	}
}
