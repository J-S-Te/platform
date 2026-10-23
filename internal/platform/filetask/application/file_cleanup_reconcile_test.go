package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

func reconcileService(t *testing.T, repository *bindingRepositoryStub, store bindingStoreStub, now time.Time) *FileService {
	t.Helper()
	service, err := NewFileService(repository, store, fixedIDGenerator{id: "01K00000000000000000000000"}, fixedClock{value: now}, DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func staleDeletion(id, retentionClass string, retention *time.Time) domain.StoredFile {
	return domain.StoredFile{
		File: domain.File{
			ID: id, TenantID: "tenant-reconcile", ApplicationID: "app-1", Status: domain.FileStatusDeleting,
			RetentionClass: retentionClass, RetentionUntil: retention, Version: 7,
		},
		Version: domain.FileVersion{ID: id[:22] + "V1" + id[25:], FileID: id, StorageRelativePath: "namespace/" + id},
	}
}

// 磁盘已删、数据库未推进：只能推进为 DELETED，绝不能恢复成可下载。
func TestReconcileStaleDeletionsFinalizesWhenBytesAlreadyGone(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	repository := &bindingRepositoryStub{stale: []domain.StoredFile{staleDeletion("01RECONCILE00000000000001", domain.RetentionClassTemporary, &past)}}
	service := reconcileService(t, repository, bindingStoreStub{missingBytes: true}, now)

	result, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.deleted) != 1 || len(repository.released) != 0 {
		t.Fatalf("deleted=%v released=%v, want finalize only", repository.deleted, repository.released)
	}
	if result.ReconciledFiles != 1 || result.DeletedFiles != 1 || result.FailedFiles != 0 {
		t.Fatalf("result=%+v, want reconciled=1 deleted=1 failed=0", result)
	}
}

// 字节仍在且仍满足"临时类 + 保留期已到"：删除并推进。
func TestReconcileStaleDeletionsDeletesWhenPolicyStillApplies(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	repository := &bindingRepositoryStub{stale: []domain.StoredFile{staleDeletion("01RECONCILE00000000000002", domain.RetentionClassTemporary, &past)}}
	service := reconcileService(t, repository, bindingStoreStub{}, now)

	result, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.deleted) != 1 || len(repository.released) != 0 || result.DeletedFiles != 1 {
		t.Fatalf("deleted=%v released=%v result=%+v", repository.deleted, repository.released, result)
	}
}

// 字节仍在但已不满足清理条件（历史误领取）：安全释放，不删除。
func TestReconcileStaleDeletionsReleasesWhenPolicyNoLongerApplies(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	for name, file := range map[string]domain.StoredFile{
		"long-term":     staleDeletion("01RECONCILE00000000000003", domain.RetentionClassLongTerm, &past),
		"no-retention":  staleDeletion("01RECONCILE00000000000004", domain.RetentionClassTemporary, nil),
		"not-yet-due":   staleDeletion("01RECONCILE00000000000005", domain.RetentionClassTemporary, ptrTime(now.Add(24*time.Hour))),
	} {
		t.Run(name, func(t *testing.T) {
			repository := &bindingRepositoryStub{stale: []domain.StoredFile{file}}
			service := reconcileService(t, repository, bindingStoreStub{}, now)
			result, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(repository.deleted) != 0 || len(repository.released) != 1 {
				t.Fatalf("deleted=%v released=%v, want release only", repository.deleted, repository.released)
			}
			if result.DeletedFiles != 0 || result.ReconciledFiles != 1 {
				t.Fatalf("result=%+v, want no deletion", result)
			}
		})
	}
}

// 版本冲突：保留 DELETING 并计入失败，不做批量重置。
func TestReconcileStaleDeletionsKeepsDeletingOnVersionConflict(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	repository := &bindingRepositoryStub{
		stale:       []domain.StoredFile{staleDeletion("01RECONCILE00000000000006", domain.RetentionClassTemporary, &past)},
		finalizeErr: ErrConflict,
	}
	service := reconcileService(t, repository, bindingStoreStub{}, now)

	result, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now, 50)
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict reported", err)
	}
	if result.FailedFiles != 1 || result.ReconciledFiles != 0 || len(repository.deleted) != 0 {
		t.Fatalf("result=%+v deleted=%v, want failure with record kept", result, repository.deleted)
	}
}

// 截止点不得晚于当前时间，批量必须有界。
func TestReconcileStaleDeletionsValidatesBounds(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	repository := &bindingRepositoryStub{}
	service := reconcileService(t, repository, bindingStoreStub{}, now)

	if _, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now.Add(time.Hour), 10); err == nil {
		t.Fatal("future cutoff must be rejected")
	}
	if _, err := service.ReconcileStaleDeletions(context.Background(), "tenant-reconcile", now, 501); err == nil {
		t.Fatal("limit above the bounded batch must be rejected")
	}
	if _, err := service.ReconcileStaleDeletions(context.Background(), "", now, 10); err == nil {
		t.Fatal("empty tenant must be rejected")
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
