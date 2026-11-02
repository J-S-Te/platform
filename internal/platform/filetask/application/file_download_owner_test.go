package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// 安全（L4b）：属主直通只对"个人文件"（尚无 ACTIVE 业务绑定）成立。
// 文件一旦绑定业务资源，上传者身份不再构成访问授权，必须与其它主体一样通过
// platform:file:download + 业务资源绑定证明；否则上传者可以打穿业务 ACL
// 下载已绑定到其它业务对象的文件。
//
// 以下用例全部使用 fake 仓储，不连库，并断言 owner 闸门确实先查询绑定、
// 且应用维度取文件归属应用而不是请求声明的应用。

type ownerGateStub struct {
	*bindingRepositoryStub
	anyCalls      int
	anyBinding    bool
	anyErr        error
	anyTenantID   string
	anyFileID     string
	anyAppID      string
	specificCalls int
	specificBound bool
	specificAppID string
	specificType  string
	specificID    string
}

func (stub *ownerGateStub) HasAnyActiveBinding(_ context.Context, tenantID, fileID, applicationID string) (bool, error) {
	stub.anyCalls++
	stub.anyTenantID, stub.anyFileID, stub.anyAppID = tenantID, fileID, applicationID
	if stub.anyErr != nil {
		return false, stub.anyErr
	}
	return stub.anyBinding, nil
}

func (stub *ownerGateStub) HasActiveBinding(_ context.Context, _, _, applicationID, resourceType, resourceID string) (bool, error) {
	stub.specificCalls++
	stub.specificAppID, stub.specificType, stub.specificID = applicationID, resourceType, resourceID
	return stub.specificBound, nil
}

func ownerGateStoredFile(content []byte) domain.StoredFile {
	digest := sha256.Sum256(content)
	return domain.StoredFile{
		File: domain.File{
			ID: "file-1", TenantID: "tenant-1", ApplicationID: "app-1", OwnerUserID: "owner-1",
			Status: domain.FileStatusReady,
		},
		Version: domain.FileVersion{
			StorageRelativePath: "tenant/file.bin", SizeBytes: uint64(len(content)), SHA256: digest[:],
			MediaType: "text/plain", Status: domain.FileVersionStatusReady,
		},
	}
}

func newOwnerGateService(t *testing.T, stub *ownerGateStub, content []byte) *FileService {
	t.Helper()
	service, err := NewFileService(stub, bindingStoreStub{content: content}, fixedIDGenerator{id: "file-1"}, fixedClock{value: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 个人文件（无 ACTIVE 绑定）：owner 直通保持有效，即使没有任何下载权限。
// 同时锁定绑定查询的应用维度取文件归属应用，不被请求声明的应用影响。
func TestOwnerPersonalFileWithoutBindingKeepsOwnerBypass(t *testing.T) {
	t.Parallel()
	content := []byte("personal file")
	stub := &ownerGateStub{bindingRepositoryStub: &bindingRepositoryStub{stored: ownerGateStoredFile(content)}}
	service := newOwnerGateService(t, stub, content)

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "owner-1", ApplicationID: "declared-app",
	}, "file-1")
	if err != nil {
		t.Fatalf("个人文件的属主下载被拒绝: %v", err)
	}
	_ = stream.Close()
	if stub.anyCalls != 1 || stub.specificCalls != 0 {
		t.Fatalf("anyCalls=%d specificCalls=%d, want 1/0", stub.anyCalls, stub.specificCalls)
	}
	if stub.anyTenantID != "tenant-1" || stub.anyFileID != "file-1" || stub.anyAppID != "app-1" {
		t.Fatalf("绑定查询维度 = (%q,%q,%q), want (tenant-1,file-1,app-1)", stub.anyTenantID, stub.anyFileID, stub.anyAppID)
	}
}

// 已绑定业务资源的文件：属主没有 platform:file:download 时不得直通。
func TestOwnerBoundFileWithoutDownloadPermissionIsForbidden(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &ownerGateStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: ownerGateStoredFile(content)},
		anyBinding:            true,
	}
	service := newOwnerGateService(t, stub, content)

	_, _, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "owner-1", ApplicationID: "app-1",
	}, "file-1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("已绑定文件的属主绕过下载权限: error = %v, want ErrForbidden", err)
	}
	if stub.anyCalls != 1 || stub.specificCalls != 0 {
		t.Fatalf("anyCalls=%d specificCalls=%d, want 1/0", stub.anyCalls, stub.specificCalls)
	}
}

// 已绑定业务资源的文件：属主有下载权限但资源声明不成立时仍然拒绝。
func TestOwnerBoundFileWithMismatchedResourceIsForbidden(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &ownerGateStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: ownerGateStoredFile(content)},
		anyBinding:            true,
		specificBound:         false,
	}
	service := newOwnerGateService(t, stub, content)

	_, _, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "owner-1", ApplicationID: "app-1", ResourceType: "CONTRACT", ResourceID: "contract-1",
		PermissionCodes: []string{"platform:file:download"}, ResourceAccessVerified: true,
	}, "file-1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("属主以不匹配的资源证明下载: error = %v, want ErrForbidden", err)
	}
	if stub.anyCalls != 1 || stub.specificCalls != 1 {
		t.Fatalf("anyCalls=%d specificCalls=%d, want 1/1", stub.anyCalls, stub.specificCalls)
	}
	if stub.specificAppID != "app-1" || stub.specificType != "CONTRACT" || stub.specificID != "contract-1" {
		t.Fatalf("资源绑定查询维度 = (%q,%q,%q)", stub.specificAppID, stub.specificType, stub.specificID)
	}
}

// 已绑定业务资源的文件：属主有下载权限且存在精确资源绑定时才放行。
func TestOwnerBoundFileWithExactResourceBindingAllowsDownload(t *testing.T) {
	t.Parallel()
	content := []byte("bound file")
	stub := &ownerGateStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: ownerGateStoredFile(content)},
		anyBinding:            true,
		specificBound:         true,
	}
	service := newOwnerGateService(t, stub, content)

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "owner-1", ApplicationID: "app-1", ResourceType: "CONTRACT", ResourceID: "contract-1",
		PermissionCodes: []string{"platform:file:download"}, ResourceAccessVerified: true,
	}, "file-1")
	if err != nil {
		t.Fatalf("属主在精确绑定存在时被拒绝: %v", err)
	}
	_ = stream.Close()
	if stub.anyCalls != 1 || stub.specificCalls != 1 {
		t.Fatalf("anyCalls=%d specificCalls=%d, want 1/1", stub.anyCalls, stub.specificCalls)
	}
}

// 绑定查询失败必须原样返回，不能被降级为放行或拒绝（失败即失败）。
func TestOwnerBindingLookupErrorPropagates(t *testing.T) {
	t.Parallel()
	content := []byte("personal file")
	lookupErr := errors.New("binding store unavailable")
	stub := &ownerGateStub{
		bindingRepositoryStub: &bindingRepositoryStub{stored: ownerGateStoredFile(content)},
		anyErr:                lookupErr,
	}
	service := newOwnerGateService(t, stub, content)

	_, stream, err := service.OpenDownload(context.Background(), DownloadAccess{
		TenantID: "tenant-1", UserID: "owner-1", ApplicationID: "app-1",
		PermissionCodes: []string{"platform:file:download"},
	}, "file-1")
	if !errors.Is(err, lookupErr) {
		t.Fatalf("绑定查询错误未原样返回: error = %v", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("绑定查询错误被改写成 ErrForbidden: %v", err)
	}
	if stream != nil {
		_ = stream.Close()
		t.Fatal("绑定查询失败时不得打开文件句柄")
	}
	if stub.anyCalls != 1 {
		t.Fatalf("anyCalls=%d, want 1", stub.anyCalls)
	}
}
