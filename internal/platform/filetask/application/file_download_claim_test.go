package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// 安全（SEC-D9）负向测试：HTTP 边界不再设置 ResourceAccessVerified 后，
// 只依据数据库中的具体绑定证明放行——"存在任意绑定"不足以通过。

type downloadClaimStub struct {
	*bindingRepositoryStub
	specificCalls int
	anyCalls      int
	specific      bool
	anyBinding    bool
}

func (stub *downloadClaimStub) HasActiveBinding(context.Context, string, string, string, string, string) (bool, error) {
	stub.specificCalls++
	return stub.specific, nil
}

func (stub *downloadClaimStub) HasAnyActiveBinding(context.Context, string, string, string) (bool, error) {
	stub.anyCalls++
	return stub.anyBinding, nil
}

func newDownloadClaimService(t *testing.T, stub *downloadClaimStub, content []byte) *FileService {
	t.Helper()
	service, err := NewFileService(stub, bindingStoreStub{content: content}, fixedIDGenerator{id: "file-1"}, fixedClock{value: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func claimStoredFile(content []byte) domain.StoredFile {
	digest := sha256.Sum256(content)
	return domain.StoredFile{
		File:    domain.File{ID: "file-1", TenantID: "tenant-1", ApplicationID: "app-1", Status: domain.FileStatusReady},
		Version: domain.FileVersion{StorageRelativePath: "tenant/file.bin", SizeBytes: uint64(len(content)), SHA256: digest[:], MediaType: "text/plain", Status: domain.FileVersionStatusReady},
	}
}

// HTTP 声明（无自证标志）只有"文件绑定了所声明的具体资源"才放行；
// 文件绑定了别的资源（any=true、specific=false）时必须拒绝。
func TestOpenDownloadHTTPClaimRequiresSpecificBinding(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &downloadClaimStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: claimStoredFile(content)},
		specific:              false,
		anyBinding:            true,
	}
	service := newDownloadClaimService(t, stub, content)

	_, _, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", ApplicationID: "app-1", ResourceType: "REPORT", ResourceID: "report-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨资源声明（仅有任意绑定）error = %v, want ErrForbidden", err)
	}
	if stub.specificCalls != 1 || stub.anyCalls != 0 {
		t.Fatalf("specificCalls=%d anyCalls=%d, want 只查具体绑定 1/0", stub.specificCalls, stub.anyCalls)
	}
}

func TestOpenDownloadHTTPClaimAllowsSpecificBinding(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &downloadClaimStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: claimStoredFile(content)},
		specific:              true,
		anyBinding:            false,
	}
	service := newDownloadClaimService(t, stub, content)

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", ApplicationID: "app-1", ResourceType: "REPORT", ResourceID: "report-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if err != nil {
		t.Fatalf("具体绑定存在时被拒绝: %v", err)
	}
	_ = stream.Close()
	if stub.specificCalls != 1 || stub.anyCalls != 0 {
		t.Fatalf("specificCalls=%d anyCalls=%d, want 1/0", stub.specificCalls, stub.anyCalls)
	}
}

// 资源标识只给一半（只有 type 或只有 id）视为半声明，直接拒绝。
func TestOpenDownloadRejectsPartialResourceClaim(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &downloadClaimStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: claimStoredFile(content)},
		specific:              true,
		anyBinding:            true,
	}
	service := newDownloadClaimService(t, stub, content)

	for _, access := range []DownloadAccess{
		{TenantID: "tenant-1", ApplicationID: "app-1", ResourceType: "REPORT", PermissionCodes: []string{"platform:file:download"}},
		{TenantID: "tenant-1", ApplicationID: "app-1", ResourceID: "report-1", PermissionCodes: []string{"platform:file:download"}},
	} {
		if _, _, err := service.OpenDownload(context.Background(), access, "file-1"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("半声明 access=%+v error = %v, want ErrForbidden", access, err)
		}
	}
	if stub.specificCalls != 0 || stub.anyCalls != 0 {
		t.Fatalf("半声明不应触达绑定查询: specific=%d any=%d", stub.specificCalls, stub.anyCalls)
	}
}

// 浏览器用户（UserID 非空）在没有受信标志时不能凭声明放行——既有语义保持不变。
func TestOpenDownloadBrowserClaimWithoutTrustedFlagStaysForbidden(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &downloadClaimStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: claimStoredFile(content)},
		specific:              true,
		anyBinding:            true,
	}
	service := newDownloadClaimService(t, stub, content)

	_, _, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "user-2", ApplicationID: "app-1", ResourceType: "REPORT", ResourceID: "report-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("浏览器自证声明 error = %v, want ErrForbidden", err)
	}
	if stub.specificCalls != 0 || stub.anyCalls != 0 {
		t.Fatalf("浏览器自证不应触达绑定查询: specific=%d any=%d", stub.specificCalls, stub.anyCalls)
	}
}

// 旧 v1 机器下载（无资源标识、无自证标志）仍走"任意 ACTIVE 绑定"兼容路径。
func TestOpenDownloadV1MachineWithoutClaimsKeepsAnyBindingCompat(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &downloadClaimStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: claimStoredFile(content)},
		specific:              false,
		anyBinding:            true,
	}
	service := newDownloadClaimService(t, stub, content)

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", ApplicationID: "app-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if err != nil {
		t.Fatalf("v1 机器下载被拒绝: %v", err)
	}
	_ = stream.Close()
	if stub.anyCalls != 1 || stub.specificCalls != 0 {
		t.Fatalf("specificCalls=%d anyCalls=%d, want 0/1", stub.specificCalls, stub.anyCalls)
	}
}
