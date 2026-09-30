package filetaskhttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// AUD-2026-022 回归：仍在路由表中的 v1 上传/下载必须产生网关访问审计。
// AUD-2026-025 回归：v1 并发超限返回 503，名额在请求结束后恢复；v2 流式路径不在此限。

type stubV1FileService struct {
	uploadErr   error
	downloadErr error
	uploaded    application.UploadInput
	file        domain.File
	block       chan struct{}
}

func (service *stubV1FileService) Upload(_ context.Context, input application.UploadInput) (domain.File, error) {
	service.uploaded = input
	if service.uploadErr != nil {
		return domain.File{}, service.uploadErr
	}
	return service.file, nil
}

func (service *stubV1FileService) OpenDownload(_ context.Context, _ application.DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	if service.block != nil {
		<-service.block
	}
	if service.downloadErr != nil {
		return domain.StoredFile{}, nil, service.downloadErr
	}
	return domain.StoredFile{
		File:    domain.File{ID: fileID, TenantID: "tenant-1", ApplicationID: "app-1", Status: domain.FileStatusReady},
		Version: domain.FileVersion{MediaType: "text/plain", OriginalName: "f.txt", Status: domain.FileVersionStatusReady, SizeBytes: 2},
	}, &nopReadSeekCloser{Reader: bytes.NewReader([]byte("ok"))}, nil
}

func (service *stubV1FileService) BindResource(context.Context, application.BindingInput) (domain.FileBinding, error) {
	return domain.FileBinding{}, errors.New("not implemented")
}
func (service *stubV1FileService) UnbindResource(context.Context, string, string, string, string) error {
	return errors.New("not implemented")
}
func (service *stubV1FileService) CleanupUnboundExpired(context.Context, string, time.Time, int) (domain.CleanupResult, error) {
	return domain.CleanupResult{}, errors.New("not implemented")
}
func (service *stubV1FileService) ReconcileStaleUploads(context.Context, string, time.Time, int) (domain.ReconcileResult, error) {
	return domain.ReconcileResult{}, errors.New("not implemented")
}

type stubV1JobService struct{}

func (stubV1JobService) Enqueue(context.Context, application.JobCreateInput) (domain.Job, error) {
	return domain.Job{}, errors.New("not implemented")
}
func (stubV1JobService) List(context.Context, string, domain.PageRequest) (domain.PageResult[domain.Job], error) {
	return domain.PageResult[domain.Job]{}, errors.New("not implemented")
}
func (stubV1JobService) Cancel(context.Context, string, string) error {
	return errors.New("not implemented")
}
func (stubV1JobService) Retry(context.Context, string, string) error {
	return errors.New("not implemented")
}
func (stubV1JobService) Rerun(context.Context, string, string) (domain.Job, error) {
	return domain.Job{}, errors.New("not implemented")
}

func v1Principal() authctx.Principal {
	return authctx.Principal{
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		Account:   authctx.ReferenceName{ID: "app-1", Code: "contract_management"},
		User:      authctx.ReferenceName{ID: "user-1"},
		SessionID: "session-1",
	}
}

func v1UploadRequest(t *testing.T) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("application_id", "app-1"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Request-ID", "req-v1-upload")
	return request.WithContext(authctx.WithPrincipal(request.Context(), v1Principal()))
}

func v1DownloadRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/content", nil)
	request.SetPathValue("file_id", "file-1")
	return request.WithContext(authctx.WithPrincipal(request.Context(), v1Principal()))
}

func TestV1UploadWritesAccessAudit(t *testing.T) {
	script := &auditScript{}
	files := &stubV1FileService{file: domain.File{ID: "file-1", OriginalName: "hello.txt", MediaType: "text/plain", Classification: "INTERNAL", Status: domain.FileStatusReady, CreatedAt: time.Now().UTC()}}
	handler, err := NewHandler(files, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.Upload(response, v1UploadRequest(t))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	inserts := script.inserts()
	if len(inserts) != 1 {
		t.Fatalf("v1 上传应产生恰好一条访问审计 INSERT，实际 %d: %v", len(inserts), script.execs)
	}
	if !strings.Contains(strings.ToUpper(inserts[0].query), "FILE_ACCESS_AUDIT") {
		t.Fatalf("审计写入目标表错误: %s", inserts[0].query)
	}
	joined := strings.Join(script.insertArgs(), "|")
	for _, expected := range []string{"UPLOAD_COMPLETED", "SUCCESS", "tenant-1", "app-1", "file-1", "user-1", "session-1", "req-v1-upload"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("v1 上传审计缺少字段值 %q: %v", expected, script.insertArgs())
		}
	}
}

func TestV1UploadFailureWritesFailedAccessAudit(t *testing.T) {
	script := &auditScript{}
	files := &stubV1FileService{uploadErr: application.ErrStorage}
	handler, err := NewHandler(files, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.Upload(response, v1UploadRequest(t))

	// 主流程错误响应保持既有错误映射（ErrStorage → 500）。
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	joined := strings.Join(script.insertArgs(), "|")
	if !strings.Contains(joined, "UPLOAD_COMPLETED") || !strings.Contains(joined, "FAILED") {
		t.Fatalf("v1 上传失败必须留 FAILED 访问审计: %v", script.insertArgs())
	}
}

func TestV1DownloadWritesAccessAudit(t *testing.T) {
	script := &auditScript{}
	handler, err := NewHandler(&stubV1FileService{}, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.Download(response, v1DownloadRequest())

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	inserts := script.inserts()
	if len(inserts) != 1 {
		t.Fatalf("v1 下载应产生恰好一条访问审计 INSERT，实际 %d: %v", len(inserts), script.execs)
	}
	if !strings.Contains(strings.ToUpper(inserts[0].query), "FILE_ACCESS_AUDIT") {
		t.Fatalf("审计写入目标表错误: %s", inserts[0].query)
	}
	joined := strings.Join(script.insertArgs(), "|")
	for _, expected := range []string{"DOWNLOAD_COMPLETED", "SUCCESS", "tenant-1", "app-1", "file-1", "user-1", "session-1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("v1 下载审计缺少字段值 %q: %v", expected, script.insertArgs())
		}
	}
}

func TestV1DownloadFailureWritesFailedAccessAudit(t *testing.T) {
	script := &auditScript{}
	handler, err := NewHandler(&stubV1FileService{downloadErr: application.ErrForbidden}, &stubV1JobService{}, slog.New(&captureLogger{}), newAuditTestDB(t, script))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	response := httptest.NewRecorder()

	handler.Download(response, v1DownloadRequest())

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	joined := strings.Join(script.insertArgs(), "|")
	if !strings.Contains(joined, "DOWNLOAD_COMPLETED") || !strings.Contains(joined, "FAILED") {
		t.Fatalf("v1 下载失败必须留 FAILED 访问审计: %v", script.insertArgs())
	}
}

// gatedDownloadService 让 OpenDownload 阻塞直到测试放行，用于构造并发在途请求。
type gatedDownloadService struct {
	entered chan struct{}
	release chan struct{}
}

func (service *gatedDownloadService) Upload(context.Context, application.UploadInput) (domain.File, error) {
	return domain.File{}, errors.New("not implemented")
}

func (service *gatedDownloadService) OpenDownload(_ context.Context, _ application.DownloadAccess, fileID string) (domain.StoredFile, io.ReadSeekCloser, error) {
	service.entered <- struct{}{}
	<-service.release
	return domain.StoredFile{
		File:    domain.File{ID: fileID, TenantID: "tenant-1", ApplicationID: "app-1", Status: domain.FileStatusReady},
		Version: domain.FileVersion{MediaType: "text/plain", OriginalName: "f.txt", Status: domain.FileVersionStatusReady},
	}, &nopReadSeekCloser{Reader: bytes.NewReader([]byte("ok"))}, nil
}

func (service *gatedDownloadService) BindResource(context.Context, application.BindingInput) (domain.FileBinding, error) {
	return domain.FileBinding{}, errors.New("not implemented")
}
func (service *gatedDownloadService) UnbindResource(context.Context, string, string, string, string) error {
	return errors.New("not implemented")
}
func (service *gatedDownloadService) CleanupUnboundExpired(context.Context, string, time.Time, int) (domain.CleanupResult, error) {
	return domain.CleanupResult{}, errors.New("not implemented")
}
func (service *gatedDownloadService) ReconcileStaleUploads(context.Context, string, time.Time, int) (domain.ReconcileResult, error) {
	return domain.ReconcileResult{}, errors.New("not implemented")
}

func TestV1DownloadConcurrencyLimitAndRecovery(t *testing.T) {
	files := &gatedDownloadService{entered: make(chan struct{}, v1MaxConcurrentFileOps+1), release: make(chan struct{})}
	handler, err := NewHandler(files, &stubV1JobService{}, slog.New(&captureLogger{}), nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	var waitGroup sync.WaitGroup
	inFlight := make([]*httptest.ResponseRecorder, v1MaxConcurrentFileOps)
	for index := 0; index < v1MaxConcurrentFileOps; index++ {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			recorder := httptest.NewRecorder()
			handler.Download(recorder, v1DownloadRequest())
			inFlight[slot] = recorder
		}(index)
	}
	// 前 4 个请求全部进入服务层并占用全部并发名额。
	for index := 0; index < v1MaxConcurrentFileOps; index++ {
		select {
		case <-files.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("并发下载未到达服务层，测试前置条件不成立")
		}
	}
	// 第 5 个并发请求必须被限流拒绝（503，既有错误信封风格）。
	fifth := httptest.NewRecorder()
	handler.Download(fifth, v1DownloadRequest())
	if fifth.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 5 个并发请求 status = %d, body = %s", fifth.Code, fifth.Body.String())
	}
	if !strings.Contains(fifth.Body.String(), "FILE_GATEWAY_BUSY") {
		t.Fatalf("第 5 个并发请求 body = %s", fifth.Body.String())
	}
	// 放行在途请求后名额恢复，新请求重新成功。
	close(files.release)
	waitGroup.Wait()
	for index, recorder := range inFlight {
		if recorder == nil || recorder.Code != http.StatusOK {
			t.Fatalf("在途下载 %d 未成功: %v", index, recorder)
		}
	}
	recovered := httptest.NewRecorder()
	handler.Download(recovered, v1DownloadRequest())
	if recovered.Code != http.StatusOK {
		t.Fatalf("名额恢复后的请求 status = %d, body = %s", recovered.Code, recovered.Body.String())
	}
}
