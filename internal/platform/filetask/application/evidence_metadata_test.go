package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

// evidenceStatRepository 在共用绑定桩之上补一个"仓储读取失败"开关，
// 用于验证只读核验在数据库故障时失败关闭而不是返回空元数据。
type evidenceStatRepository struct {
	*bindingRepositoryStub
	getAvailableErr error
}

func (stub *evidenceStatRepository) GetAvailable(ctx context.Context, tenantID, fileID string) (domain.StoredFile, error) {
	if stub.getAvailableErr != nil {
		return domain.StoredFile{}, stub.getAvailableErr
	}
	return stub.bindingRepositoryStub.GetAvailable(ctx, tenantID, fileID)
}

const evidenceStatFileID = "file-evidence-1"

func evidenceStatFixture() (*evidenceStatRepository, domain.StoredFile, string) {
	content := []byte("field evidence")
	digest := sha256.Sum256(content)
	stored := domain.StoredFile{
		File: domain.File{
			ID: evidenceStatFileID, TenantID: "tenant-1", ApplicationID: "app-project",
			Status: domain.FileStatusReady, CreatedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
		},
		Version: domain.FileVersion{
			StorageRelativePath: "project/field-evidence/tenant-1/2026/10/file-evidence-1/v1/content",
			SizeBytes:           uint64(len(content)), SHA256: digest[:],
			MediaType: "image/png", OriginalName: "field.png", Status: domain.FileVersionStatusReady,
		},
	}
	repository := &evidenceStatRepository{bindingRepositoryStub: &bindingRepositoryStub{stored: stored, bound: true}}
	return repository, stored, hex.EncodeToString(digest[:])
}

func newEvidenceStatService(t *testing.T, repository *evidenceStatRepository) *FileService {
	t.Helper()
	service, err := NewFileService(repository, bindingStoreStub{content: []byte("field evidence")}, fixedIDGenerator{id: "evidence-binding-1"}, fixedClock{value: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}, DefaultUploadPolicy())
	if err != nil {
		t.Fatalf("NewFileService: %v", err)
	}
	return service
}

const (
	evidenceStatResourceType = "PROJECT_SERVICE_ITEM"
	evidenceStatResourceID   = "SI-EVIDENCE-1"
)

func evidenceStatAccess() EvidenceAccess {
	return EvidenceAccess{
		TenantID: "tenant-1", ApplicationID: "app-project",
		ResourceType: evidenceStatResourceType, ResourceID: evidenceStatResourceID,
		PermissionCodes: []string{"platform:file:download"},
	}
}

// TestStatEvidenceReturnsVerifiedMetadata 覆盖正常路径：元数据必须来自服务端解析到的
// 文件与当前版本，而不是调用方声明。
func TestStatEvidenceReturnsVerifiedMetadata(t *testing.T) {
	t.Parallel()
	repository, stored, digest := evidenceStatFixture()
	service := newEvidenceStatService(t, repository)

	metadata, err := service.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID)
	if err != nil {
		t.Fatalf("StatEvidence returned error: %v", err)
	}
	if metadata.FileID != stored.File.ID || metadata.TenantID != "tenant-1" || metadata.ApplicationID != "app-project" {
		t.Fatalf("identity mismatch: %+v", metadata)
	}
	if metadata.SHA256 != digest || metadata.SizeBytes != stored.Version.SizeBytes {
		t.Fatalf("content identity mismatch: %+v want sha=%s size=%d", metadata, digest, stored.Version.SizeBytes)
	}
	if metadata.MediaType != "image/png" || metadata.OriginalName != "field.png" {
		t.Fatalf("media identity mismatch: %+v", metadata)
	}
	if metadata.Status != domain.FileStatusReady || !metadata.BindingActive {
		t.Fatalf("status/binding mismatch: %+v", metadata)
	}
	if !metadata.CreatedAt.Equal(stored.File.CreatedAt) {
		t.Fatalf("created_at = %s want %s", metadata.CreatedAt, stored.File.CreatedAt)
	}
	// 绑定必须按业务对象精确下推到仓储：只查"该文件存在任意绑定"无法阻止跨服务项复用。
	if repository.exactBindingCalls != 1 || repository.exactResourceType != evidenceStatResourceType || repository.exactResourceID != evidenceStatResourceID {
		t.Fatalf("binding query = calls:%d type:%q id:%q", repository.exactBindingCalls, repository.exactResourceType, repository.exactResourceID)
	}
}

// TestStatEvidenceHidesOtherTenantsAndApplications 断言跨租户、跨应用与真正不存在
// 得到同一个回答，调用方无法借该端点探测其它租户/应用是否存在某个文件。
func TestStatEvidenceHidesOtherTenantsAndApplications(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		file domain.File
	}{
		{name: "other tenant", file: domain.File{ID: evidenceStatFileID, TenantID: "tenant-2", ApplicationID: "app-project", Status: domain.FileStatusReady}},
		{name: "other application", file: domain.File{ID: evidenceStatFileID, TenantID: "tenant-1", ApplicationID: "app-contract", Status: domain.FileStatusReady}},
		{name: "missing file", file: domain.File{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository, _, _ := evidenceStatFixture()
			repository.bindingRepositoryStub.stored.File = testCase.file
			service := newEvidenceStatService(t, repository)
			if _, err := service.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestStatEvidenceRejectsReuseByAnotherServiceItem 覆盖本次修复的核心回归：
// 同一应用内，服务项 A 的证据文件被服务项 B 复用时，网关必须回到"无该对象绑定"并拒绝。
func TestStatEvidenceRejectsReuseByAnotherServiceItem(t *testing.T) {
	t.Parallel()
	repository, _, _ := evidenceStatFixture()
	// 文件中存在 ACTIVE 绑定（任意绑定），但只绑定到服务项 A，不绑定到调用方声明的服务项 B。
	repository.bound = true
	boundElsewhere := false
	repository.exactBound = &boundElsewhere
	service := newEvidenceStatService(t, repository)
	otherItemAccess := evidenceStatAccess()
	otherItemAccess.ResourceID = "SI-EVIDENCE-2"
	if _, err := service.StatEvidence(context.Background(), otherItemAccess, evidenceStatFileID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-item reuse error = %v, want ErrConflict", err)
	}
	if repository.exactResourceID != "SI-EVIDENCE-2" {
		t.Fatalf("binding query resource id = %q", repository.exactResourceID)
	}
	// 同一文件绑定到当前服务项时才能通过：对象级绑定是唯一放行依据。
	boundHere := true
	repository.exactBound = &boundHere
	if _, err := service.StatEvidence(context.Background(), otherItemAccess, evidenceStatFileID); err != nil {
		t.Fatalf("exact object binding rejected: %v", err)
	}
}

// TestStatEvidenceRequiresActiveBinding 断言没有 ACTIVE 业务绑定的文件不能当证据，
// 否则任何"可下载但未绑定"的文件都能冒充现场证据。
func TestStatEvidenceRequiresActiveBinding(t *testing.T) {
	t.Parallel()
	repository, _, _ := evidenceStatFixture()
	repository.bindingRepositoryStub.bound = false
	service := newEvidenceStatService(t, repository)
	if _, err := service.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
}

// TestStatEvidenceFailsClosedOnUnavailableStorageState 断言仓储不可用或文件不可用时
// 核验失败关闭：不返回任何可被当作"已核验"的元数据。
func TestStatEvidenceFailsClosedOnUnavailableStorageState(t *testing.T) {
	t.Parallel()
	repository, _, _ := evidenceStatFixture()
	repository.getAvailableErr = errors.New("database is unavailable")
	service := newEvidenceStatService(t, repository)
	if _, err := service.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("storage failure must not be reported as a not-found success path: %v", err)
	}

	// 文件存在但没有可用版本（仓储返回 ErrNotFound）同样不能核验成功。
	unavailable := &evidenceStatRepository{bindingRepositoryStub: &bindingRepositoryStub{bound: true}, getAvailableErr: ErrNotFound}
	unavailableService := newEvidenceStatService(t, unavailable)
	if _, err := unavailableService.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unavailable file error = %v, want ErrNotFound", err)
	}
}

// TestStatEvidenceRequiresDownloadScopeAndIdentifiers 断言请求缺少下载权限或标识时
// 直接拒绝，不会退化成"空元数据成功"。
func TestStatEvidenceRequiresDownloadScopeAndIdentifiers(t *testing.T) {
	t.Parallel()
	repository, _, _ := evidenceStatFixture()
	service := newEvidenceStatService(t, repository)
	withoutScope := evidenceStatAccess()
	withoutScope.PermissionCodes = []string{"platform:file:upload"}
	if _, err := service.StatEvidence(context.Background(), withoutScope, evidenceStatFileID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing download scope error = %v, want ErrForbidden", err)
	}
	if _, err := service.StatEvidence(context.Background(), evidenceStatAccess(), "   "); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing file id error = %v, want ErrValidation", err)
	}
	withoutTenant := evidenceStatAccess()
	withoutTenant.TenantID = ""
	if _, err := service.StatEvidence(context.Background(), withoutTenant, evidenceStatFileID); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing tenant error = %v, want ErrValidation", err)
	}
	// 资源标识是对象级绑定的输入：缺失或超长都不能退化成"仅校验应用级绑定"。
	for _, broken := range []EvidenceAccess{
		func() EvidenceAccess { access := evidenceStatAccess(); access.ResourceType = "  "; return access }(),
		func() EvidenceAccess { access := evidenceStatAccess(); access.ResourceID = ""; return access }(),
		func() EvidenceAccess {
			access := evidenceStatAccess()
			access.ResourceID = strings.Repeat("x", evidenceResourceIDMaxLen+1)
			return access
		}(),
	} {
		if _, err := service.StatEvidence(context.Background(), broken, evidenceStatFileID); !errors.Is(err, ErrValidation) {
			t.Fatalf("access %+v error = %v, want ErrValidation", broken, err)
		}
	}
	if repository.exactBindingCalls != 0 {
		t.Fatalf("invalid requests reached the binding repository %d times", repository.exactBindingCalls)
	}
}

// TestStatEvidencePassesBindingTypeToRepository 断言可选的 binding_type 原样下推，
// 未给出时仓储侧只按资源维度过滤（空值不额外收窄）。
func TestStatEvidencePassesBindingTypeToRepository(t *testing.T) {
	t.Parallel()
	repository, _, _ := evidenceStatFixture()
	service := newEvidenceStatService(t, repository)
	access := evidenceStatAccess()
	access.BindingType = "SIGNATURE_EVIDENCE"
	if _, err := service.StatEvidence(context.Background(), access, evidenceStatFileID); err != nil {
		t.Fatalf("StatEvidence: %v", err)
	}
	if repository.exactBindingType != "SIGNATURE_EVIDENCE" {
		t.Fatalf("binding type = %q", repository.exactBindingType)
	}
	if _, err := service.StatEvidence(context.Background(), evidenceStatAccess(), evidenceStatFileID); err != nil {
		t.Fatalf("StatEvidence without binding type: %v", err)
	}
	if repository.exactBindingType != "" {
		t.Fatalf("binding type = %q want empty", repository.exactBindingType)
	}
}
