package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

type bindingRepositoryStub struct {
	stored             domain.StoredFile
	bound              bool
	created            domain.FileBinding
	recoveryCandidates []domain.StoredFile
	validatingCount    int
	readyCount         int
	// 上传会话恢复用例：与待对账文件关联的 UPLOADING 会话及其收敛记录。
	pendingSession      *domain.UploadSessionRecovery
	sessionTransitions  []string
	sessionFailureCodes []string
	bindingErr          error
	// 受控对账用例使用：待对账的遗留 DELETING 记录与写入调用记录。
	stale       []domain.StoredFile
	deleted     []string
	released    []string
	finalizeErr error
}

func (stub *bindingRepositoryStub) ListStaleDeleting(context.Context, string, time.Time, int) ([]domain.StoredFile, error) {
	return stub.stale, nil
}

func (stub *bindingRepositoryStub) CreateWriting(context.Context, domain.File, domain.FileVersion) error {
	return nil
}
func (stub *bindingRepositoryStub) ReserveUpload(_ context.Context, file domain.File, version domain.FileVersion) (domain.StoredFile, bool, error) {
	return domain.StoredFile{File: file, Version: version}, true, nil
}
func (stub *bindingRepositoryStub) MarkValidating(context.Context, string, string, uint64, []byte, time.Time) error {
	stub.validatingCount++
	return nil
}
func (stub *bindingRepositoryStub) MarkReady(context.Context, string, string, time.Time) error {
	stub.readyCount++
	return nil
}
func (stub *bindingRepositoryStub) MarkRejected(context.Context, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) MarkFailed(context.Context, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) GetAvailable(context.Context, string, string) (domain.StoredFile, error) {
	return stub.stored, nil
}
func (stub *bindingRepositoryStub) ClaimExpiredUnbound(context.Context, string, time.Time, time.Time) (domain.StoredFile, bool, error) {
	return domain.StoredFile{}, false, nil
}
func (stub *bindingRepositoryStub) MarkDeleted(_ context.Context, _ string, fileID string, _ uint64, _ time.Time) error {
	if stub.finalizeErr != nil {
		return stub.finalizeErr
	}
	stub.deleted = append(stub.deleted, fileID)
	return nil
}
func (stub *bindingRepositoryStub) ReleaseCleanupClaim(_ context.Context, _ string, fileID, _ string, _ uint64, _ time.Time) error {
	stub.released = append(stub.released, fileID)
	return nil
}
func (stub *bindingRepositoryStub) CreateBinding(_ context.Context, binding domain.FileBinding) (domain.FileBinding, error) {
	if stub.bindingErr != nil {
		return domain.FileBinding{}, stub.bindingErr
	}
	stub.created = binding
	return binding, nil
}
func (stub *bindingRepositoryStub) DeactivateBinding(context.Context, string, string, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) HasActiveBinding(context.Context, string, string, string, string, string) (bool, error) {
	return stub.bound, nil
}
func (stub *bindingRepositoryStub) HasAnyActiveBinding(context.Context, string, string, string) (bool, error) {
	return stub.bound, nil
}
func (stub *bindingRepositoryStub) ListRecoveryCandidates(context.Context, string, time.Time, int) ([]domain.StoredFile, error) {
	return stub.recoveryCandidates, nil
}
func (stub *bindingRepositoryStub) PendingUploadSession(context.Context, string, string) (domain.UploadSessionRecovery, bool, error) {
	if stub.pendingSession == nil {
		return domain.UploadSessionRecovery{}, false, nil
	}
	return *stub.pendingSession, true, nil
}
func (stub *bindingRepositoryStub) MarkUploadSession(_ context.Context, _, _ string, status, failureCode string, _ time.Time) error {
	stub.sessionTransitions = append(stub.sessionTransitions, status)
	stub.sessionFailureCodes = append(stub.sessionFailureCodes, failureCode)
	return nil
}

type bindingStoreStub struct {
	content []byte
	// missingBytes 模拟"磁盘已删、数据库未推进"的遗留 DELETING。
	missingBytes bool
}

func (stub bindingStoreStub) Exists(string) (bool, error) { return !stub.missingBytes, nil }

func (bindingStoreStub) WriteAtomically(context.Context, string, io.Reader, int64) (uint64, []byte, error) {
	return 0, nil, errors.New("not implemented")
}
func (stub bindingStoreStub) OpenVerified(string) (io.ReadSeekCloser, error) {
	return &readSeekCloser{Reader: bytes.NewReader(stub.content)}, nil
}
func (bindingStoreStub) Remove(string) error                     { return nil }
func (bindingStoreStub) CleanupTemporary(time.Time) (int, error) { return 0, nil }

type readSeekCloser struct{ *bytes.Reader }

func (*readSeekCloser) Close() error { return nil }

type fixedIDGenerator struct{ id string }

func (generator fixedIDGenerator) New(time.Time) (string, error) { return generator.id, nil }

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

func newBindingTestService(t *testing.T, repository *bindingRepositoryStub, content []byte) *FileService {
	t.Helper()
	service, err := NewFileService(repository, bindingStoreStub{content: content}, fixedIDGenerator{id: "binding-1"}, fixedClock{value: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestOpenDownloadRequiresTrustedResourceAuthorizationForNonOwner(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	digest := sha256.Sum256(content)
	repository := &bindingRepositoryStub{bound: true, stored: domain.StoredFile{
		File:    domain.File{ID: "file-1", TenantID: "tenant-1", ApplicationID: "app-1", OwnerUserID: "owner-1", Status: domain.FileStatusReady},
		Version: domain.FileVersion{StorageRelativePath: "tenant/file.bin", SizeBytes: uint64(len(content)), SHA256: digest[:], MediaType: "text/plain", Status: domain.FileVersionStatusReady},
	}}
	service := newBindingTestService(t, repository, content)

	_, _, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "user-2", ApplicationID: "app-1", ResourceType: "REPORT", ResourceID: "report-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("browser-declared resource accepted without trusted ACL proof: %v", err)
	}

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "user-2", ApplicationID: "app-1", ResourceType: "REPORT", ResourceID: "report-1",
		PermissionCodes: []string{"platform:file:download"}, ResourceAccessVerified: true,
	}, "file-1")
	if err != nil {
		t.Fatalf("trusted bound resource rejected: %v", err)
	}
	_ = stream.Close()
}

func TestBindResourcePersistsNormalizedActiveBinding(t *testing.T) {
	t.Parallel()
	repository := &bindingRepositoryStub{}
	service := newBindingTestService(t, repository, nil)
	binding, err := service.BindResource(context.Background(), BindingInput{
		TenantID: " tenant-1 ", ApplicationID: " app-1 ", FileID: " file-1 ", ResourceType: " REPORT ",
		ResourceID: " report-1 ", BindingType: " ATTACHMENT ", DisplayName: " 报告附件 ", OperatorUserID: " user-1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.Status != "ACTIVE" || repository.created.ResourceType != "REPORT" || repository.created.DisplayName != "报告附件" {
		t.Fatalf("unexpected binding: %#v", binding)
	}
}

func TestReconcileStalePendingUploadContinuesFromDurableFile(t *testing.T) {
	t.Parallel()
	content := []byte("recovered content")
	repository := &bindingRepositoryStub{recoveryCandidates: []domain.StoredFile{{
		File:    domain.File{ID: "file-1", TenantID: "tenant-1", Status: domain.FileStatusPendingUpload},
		Version: domain.FileVersion{StorageRelativePath: "tenant/file.bin", MediaType: "text/plain", Status: domain.FileVersionStatusPendingUpload},
	}}}
	service := newBindingTestService(t, repository, content)
	result, err := service.ReconcileStaleUploads(context.Background(), "tenant-1", time.Now().UTC().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovered != 1 || repository.validatingCount != 1 || repository.readyCount != 1 {
		t.Fatalf("pending file not recovered: result=%#v validating=%d ready=%d", result, repository.validatingCount, repository.readyCount)
	}
}

// V2 上传把文件对象与上传会话分别落库。对账恢复只把文件推进到 READY 时，会话会永久停在
// UPLOADING（客户端轮询不到结果），业务绑定也不会发生。以下用例锁定"文件与会话一起收敛"。

func newRecoverableUploadFile() domain.StoredFile {
	return domain.StoredFile{
		File:    domain.File{ID: "file-1", TenantID: "tenant-1", Status: domain.FileStatusPendingUpload},
		Version: domain.FileVersion{StorageRelativePath: "tenant/file.bin", MediaType: "text/plain", Status: domain.FileVersionStatusPendingUpload},
	}
}

func newPendingUploadSession() *domain.UploadSessionRecovery {
	return &domain.UploadSessionRecovery{
		SessionID: "session-1", TenantID: "tenant-1", ApplicationID: "app-1", ActorUserID: "user-1",
		FileID: "file-1", ResourceType: "REPORT", ResourceID: "report-1", BindingType: "ATTACHMENT",
		DisplayName: "季度报告", OriginalName: "report.txt",
	}
}

func TestReconcileStaleUploadFinalizesSessionAndBinding(t *testing.T) {
	t.Parallel()
	repository := &bindingRepositoryStub{
		recoveryCandidates: []domain.StoredFile{newRecoverableUploadFile()},
		pendingSession:     newPendingUploadSession(),
	}
	service := newBindingTestService(t, repository, []byte("recovered content"))

	result, err := service.ReconcileStaleUploads(context.Background(), "tenant-1", time.Now().UTC().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1", result.Recovered)
	}
	if len(repository.sessionTransitions) != 1 || repository.sessionTransitions[0] != "READY" {
		t.Fatalf("上传会话未被收敛到 READY：%#v", repository.sessionTransitions)
	}
	if repository.created.FileID != "file-1" || repository.created.ResourceID != "report-1" ||
		repository.created.BindingType != "ATTACHMENT" || repository.created.CreatedBy != "user-1" {
		t.Fatalf("恢复后未按会话元数据补做绑定：%#v", repository.created)
	}
}

func TestReconcileStaleUploadMarksSessionFailedWhenBindingFails(t *testing.T) {
	t.Parallel()
	repository := &bindingRepositoryStub{
		recoveryCandidates: []domain.StoredFile{newRecoverableUploadFile()},
		pendingSession:     newPendingUploadSession(),
		bindingErr:         errors.New("binding store unavailable"),
	}
	service := newBindingTestService(t, repository, []byte("recovered content"))

	if _, err := service.ReconcileStaleUploads(context.Background(), "tenant-1", time.Now().UTC().Add(-time.Minute), 10); err != nil {
		t.Fatal(err)
	}
	if len(repository.sessionTransitions) != 1 || repository.sessionTransitions[0] != "FAILED" ||
		repository.sessionFailureCodes[0] != "BINDING_FAILED" {
		t.Fatalf("绑定失败时应把会话置为 FAILED(BINDING_FAILED)：%#v %#v", repository.sessionTransitions, repository.sessionFailureCodes)
	}
}

func TestReconcileStaleUploadMarksSessionFailedWhenStorageMissing(t *testing.T) {
	t.Parallel()
	repository := &bindingRepositoryStub{
		recoveryCandidates: []domain.StoredFile{newRecoverableUploadFile()},
		pendingSession:     newPendingUploadSession(),
	}
	service, err := NewFileService(repository, failingOpenStore{}, fixedIDGenerator{id: "binding-1"}, fixedClock{value: time.Now().UTC()}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.ReconcileStaleUploads(context.Background(), "tenant-1", time.Now().UTC().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 {
		t.Fatalf("failed = %d, want 1", result.Failed)
	}
	if len(repository.sessionTransitions) != 1 || repository.sessionTransitions[0] != "FAILED" ||
		repository.sessionFailureCodes[0] != "RECOVERY_FAILED" {
		t.Fatalf("存储不可用时会话应置为 FAILED(RECOVERY_FAILED)：%#v %#v", repository.sessionTransitions, repository.sessionFailureCodes)
	}
}

// 没有关联会话（V1 上传、会话已清理）时对账必须保持原有行为。
func TestReconcileStaleUploadWithoutSessionKeepsPreviousBehaviour(t *testing.T) {
	t.Parallel()
	repository := &bindingRepositoryStub{recoveryCandidates: []domain.StoredFile{newRecoverableUploadFile()}}
	service := newBindingTestService(t, repository, []byte("recovered content"))

	result, err := service.ReconcileStaleUploads(context.Background(), "tenant-1", time.Now().UTC().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovered != 1 || repository.readyCount != 1 || len(repository.sessionTransitions) != 0 {
		t.Fatalf("无会话时不应触碰会话：result=%#v ready=%d transitions=%#v", result, repository.readyCount, repository.sessionTransitions)
	}
}

type failingOpenStore struct{ bindingStoreStub }

func (failingOpenStore) OpenVerified(string) (io.ReadSeekCloser, error) {
	return nil, errors.New("object missing")
}
