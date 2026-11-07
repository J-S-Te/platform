package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// 保留期清理后的下载语义：DELETED/REMOVED 版本返回 410（曾存在但不可再获取），
// 从未完成或不属于该租户的任务仍返回 404。下载现在必须携带一次性票据，因此这里的
// 夹具直接种入票据，模拟“签发后、兑换前产物被清理”的竞态。
func TestDownloadExportReportsCleanedArtifactAsGone(t *testing.T) {
	handler, _, repository := newExportDownloadHandler(t, domain.ExportFile{
		FileID:              "file-1",
		StorageRelativePath: "tenant-1/app-1/2026/01/file-1/version-1.csv",
		OriginalName:        "audit-export-job-1.csv",
		MediaType:           "text/csv; charset=utf-8",
		Status:              "DELETED",
	})
	repository.addTicket("tenant-1", "job-1", "ticket-for-cleaned-artifact", testTicketExpiry())
	response := httptest.NewRecorder()

	handler.DownloadExport(response, exportDownloadRequest("job-1", "ticket-for-cleaned-artifact"))

	if response.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "PLATFORM_GONE") {
		t.Fatalf("body = %s, want PLATFORM_GONE error code", response.Body.String())
	}
}

func TestDownloadExportReturnsNotFoundWhenFileIsMissing(t *testing.T) {
	handler, _, repository := newExportDownloadHandler(t, domain.ExportFile{
		FileID:              "file-1",
		StorageRelativePath: "tenant-1/app-1/2026/01/file-1/version-1.csv",
		OriginalName:        "audit-export-job-1.csv",
		MediaType:           "text/csv; charset=utf-8",
		Status:              "ACTIVE",
	})
	repository.addTicket("tenant-1", "job-1", "ticket-for-missing-file", testTicketExpiry())
	response := httptest.NewRecorder()

	handler.DownloadExport(response, exportDownloadRequest("job-1", "ticket-for-missing-file"))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for missing file; body=%s", response.Code, response.Body.String())
	}
}

func TestDownloadExportServesAvailableArtifact(t *testing.T) {
	relativePath := filepath.ToSlash(filepath.Join("tenant-1", "app-1", "2026", "01", "file-1", "version-1.csv"))
	content := "event_id\n"
	digest := sha256.Sum256([]byte(content))
	handler, storageRoot, repository := newExportDownloadHandler(t, domain.ExportFile{
		FileID:              "file-1",
		StorageRelativePath: relativePath,
		OriginalName:        "audit-export-job-1.csv",
		MediaType:           "text/csv; charset=utf-8",
		Status:              "ACTIVE",
		SizeBytes:           uint64(len(content)),
		SHA256:              digest[:],
	})
	seedExportFile(t, storageRoot, relativePath, content)
	repository.addTicket("tenant-1", "job-1", "ticket-for-available-artifact", testTicketExpiry())

	response := httptest.NewRecorder()
	handler.DownloadExport(response, exportDownloadRequest("job-1", "ticket-for-available-artifact"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "event_id") {
		t.Fatalf("body = %s, want export content", response.Body.String())
	}
}

func newExportDownloadHandler(t *testing.T, file domain.ExportFile) (*Handler, string, *exportDownloadRepositoryStub) {
	t.Helper()
	storageRoot := t.TempDir()
	repository := &exportDownloadRepositoryStub{file: file}
	service, err := application.NewService(repository, exportDownloadIDs{}, exportDownloadClock{})
	if err != nil {
		t.Fatalf("new audit service: %v", err)
	}
	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), storageRoot)
	if err != nil {
		t.Fatalf("new audit handler: %v", err)
	}
	return handler, storageRoot, repository
}

// seedExportFile 在测试存储根下写入导出产物。
func seedExportFile(t *testing.T, storageRoot, relativePath, content string) {
	t.Helper()
	absolutePath := filepath.Join(storageRoot, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		t.Fatalf("create export dir: %v", err)
	}
	if err := os.WriteFile(absolutePath, []byte(content), 0o600); err != nil {
		t.Fatalf("seed export file: %v", err)
	}
}

func exportDownloadRequest(jobID, ticket string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/export-jobs/"+jobID+"/download", nil)
	request.SetPathValue("job_id", jobID)
	if ticket != "" {
		request.Header.Set(exportDownloadTicketHeader, ticket)
	}
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"}}))
}

// exportDownloadTicketRequest 构造签发票据请求；jobID 通过路由参数传入。
func exportDownloadTicketRequest(jobID string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/audit/export-jobs/"+jobID+"/download-tickets", nil)
	request.SetPathValue("job_id", jobID)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"}}))
}

func testTicketExpiry() time.Time {
	return exportDownloadClock{}.Now().Add(application.ExportDownloadTicketTTL)
}

type exportDownloadIDs struct{}

func (exportDownloadIDs) New(time.Time) (string, error) { return "01J00000000000000000000TST", nil }

type exportDownloadClock struct{}

func (exportDownloadClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// exportDownloadRepositoryStub 让 GetExportFile 返回夹具文件，并用内存票据表模拟
// 000139 的原子核销语义：同一票据的并发兑换最多一个成功。
type exportDownloadRepositoryStub struct {
	mu      sync.Mutex
	file    domain.ExportFile
	fileErr error
	tickets map[string]exportDownloadTicketRecord
}

type exportDownloadTicketRecord struct {
	tokenHash []byte
	expiresAt time.Time
	usedAt    *time.Time
}

func ticketKey(tenantID, jobID string, tokenHash []byte) string {
	return tenantID + "\x00" + jobID + "\x00" + hex.EncodeToString(tokenHash)
}

// addTicket 直接种入一张已签发的票据，供下载路径测试使用。
func (stub *exportDownloadRepositoryStub) addTicket(tenantID, jobID, token string, expiresAt time.Time) {
	digest := sha256.Sum256([]byte(token))
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tickets == nil {
		stub.tickets = make(map[string]exportDownloadTicketRecord)
	}
	stub.tickets[ticketKey(tenantID, jobID, digest[:])] = exportDownloadTicketRecord{tokenHash: digest[:], expiresAt: expiresAt}
}

// heldTokenHashes 返回当前票据表里的全部摘要，用于断言明文 token 从未入库。
func (stub *exportDownloadRepositoryStub) heldTokenHashes() [][]byte {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	hashes := make([][]byte, 0, len(stub.tickets))
	for _, record := range stub.tickets {
		hashes = append(hashes, record.tokenHash)
	}
	return hashes
}

func (stub *exportDownloadRepositoryStub) CreateExportDownloadTicket(_ context.Context, ticket domain.ExportDownloadTicket) error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tickets == nil {
		stub.tickets = make(map[string]exportDownloadTicketRecord)
	}
	stub.tickets[ticketKey(ticket.TenantID, ticket.JobID, ticket.TokenHash)] = exportDownloadTicketRecord{tokenHash: append([]byte(nil), ticket.TokenHash...), expiresAt: ticket.ExpiresAt}
	return nil
}

func (stub *exportDownloadRepositoryStub) ConsumeExportDownloadTicket(_ context.Context, tenantID, jobID string, tokenHash []byte, now time.Time) (domain.ExportDownloadTicketState, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	key := ticketKey(tenantID, jobID, tokenHash)
	record, ok := stub.tickets[key]
	if !ok {
		return domain.ExportDownloadTicketInvalid, nil
	}
	if record.usedAt != nil {
		return domain.ExportDownloadTicketInvalid, nil
	}
	if !record.expiresAt.After(now) {
		return domain.ExportDownloadTicketExpired, nil
	}
	usedAt := now
	record.usedAt = &usedAt
	stub.tickets[key] = record
	return domain.ExportDownloadTicketConsumed, nil
}

func (stub *exportDownloadRepositoryStub) GetExportFile(context.Context, string, string) (domain.ExportFile, error) {
	if stub.fileErr != nil {
		return domain.ExportFile{}, stub.fileErr
	}
	return stub.file, nil
}
func (stub *exportDownloadRepositoryStub) Ingest(context.Context, string, application.EventInput, time.Time) (domain.Receipt, error) {
	return domain.Receipt{}, nil
}
func (stub *exportDownloadRepositoryStub) IngestBatch(context.Context, string, application.BatchDeliveryInput, []application.EventInput, time.Time) ([]domain.Receipt, error) {
	return nil, nil
}
func (stub *exportDownloadRepositoryStub) List(context.Context, string, application.PageRequest) (application.PageResult[domain.Event], error) {
	return application.PageResult[domain.Event]{}, nil
}
func (stub *exportDownloadRepositoryStub) Get(context.Context, string, string) (domain.Event, error) {
	return domain.Event{}, application.ErrNotFound
}
func (stub *exportDownloadRepositoryStub) IngestionHealth(context.Context, string, time.Time) (application.IngestionHealth, error) {
	return application.IngestionHealth{}, nil
}
func (stub *exportDownloadRepositoryStub) CreateExportJob(context.Context, string, string, application.PageRequest, string, time.Time) (domain.ExportJob, error) {
	return domain.ExportJob{}, nil
}
func (stub *exportDownloadRepositoryStub) GetExportJob(context.Context, string, string) (domain.ExportJob, error) {
	return domain.ExportJob{}, application.ErrNotFound
}
func (stub *exportDownloadRepositoryStub) ClaimExportJob(context.Context, string, time.Time, time.Time) (domain.ExportWork, bool, error) {
	return domain.ExportWork{}, false, nil
}
func (stub *exportDownloadRepositoryStub) ListExportEvents(context.Context, string, domain.ExportQuery, domain.ExportCursor, int) (domain.ExportEventPage, error) {
	return domain.ExportEventPage{}, nil
}
func (stub *exportDownloadRepositoryStub) CompleteExportJob(context.Context, domain.ExportWork, domain.ExportFile, time.Time) error {
	return nil
}
func (stub *exportDownloadRepositoryStub) FailExportJob(context.Context, domain.ExportWork, string, string, time.Time, time.Time) error {
	return nil
}
func (stub *exportDownloadRepositoryStub) ListExpiredExportArtifacts(context.Context, time.Time, int) ([]domain.ExportArtifact, error) {
	return nil, nil
}
func (stub *exportDownloadRepositoryStub) MarkExportArtifactsDeleted(context.Context, []string) error {
	return nil
}
func (stub *exportDownloadRepositoryStub) VerifyPayloadHash(context.Context, string, string, []byte) (domain.PayloadHashVerification, error) {
	return domain.PayloadHashVerification{}, nil
}
func (stub *exportDownloadRepositoryStub) VerifyEventChain(context.Context, string, string, string, uint64, int) (domain.ChainVerification, error) {
	return domain.ChainVerification{}, nil
}
