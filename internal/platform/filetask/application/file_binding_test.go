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
func (stub *bindingRepositoryStub) ClaimExpiredUnbound(context.Context, string, time.Time) (domain.StoredFile, bool, error) {
	return domain.StoredFile{}, false, nil
}
func (stub *bindingRepositoryStub) MarkDeleted(context.Context, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) ReleaseCleanupClaim(context.Context, string, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) CreateBinding(_ context.Context, binding domain.FileBinding) (domain.FileBinding, error) {
	stub.created = binding
	return binding, nil
}
func (stub *bindingRepositoryStub) DeactivateBinding(context.Context, string, string, string, string, time.Time) error {
	return nil
}
func (stub *bindingRepositoryStub) HasActiveBinding(context.Context, string, string, string, string, string) (bool, error) {
	return stub.bound, nil
}
func (stub *bindingRepositoryStub) ListRecoveryCandidates(context.Context, string, time.Time, int) ([]domain.StoredFile, error) {
	return stub.recoveryCandidates, nil
}

type bindingStoreStub struct{ content []byte }

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
