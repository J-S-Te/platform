package application

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 保留期清理把 file_version 标记为 DELETED/REMOVED 后，下载元数据查询必须返回
// ErrExpired（HTTP 410），而不是继续给出可下载路径或退化成“从未存在”的 404。
func TestGetExportFileReportsCleanedArtifactAsExpired(t *testing.T) {
	for _, status := range []string{"DELETED", "REMOVED", " deleted "} {
		t.Run(status, func(t *testing.T) {
			stub := &verifyRepositoryStub{exportFile: domain.ExportFile{FileID: "file-1", StorageRelativePath: "tenant/app/2026/01/file/version.csv", Status: status}}
			service := newVerifyService(t, stub)
			if _, err := service.GetExportFile(context.Background(), "tenant-1", "job-1"); !errors.Is(err, ErrExpired) {
				t.Fatalf("GetExportFile error = %v, want ErrExpired", err)
			}
		})
	}
}

func TestGetExportFileReturnsAvailableArtifact(t *testing.T) {
	for _, status := range []string{"ACTIVE", "READY", ""} {
		t.Run(status, func(t *testing.T) {
			stub := &verifyRepositoryStub{exportFile: domain.ExportFile{FileID: "file-1", StorageRelativePath: "tenant/app/2026/01/file/version.csv", Status: status}}
			service := newVerifyService(t, stub)
			file, err := service.GetExportFile(context.Background(), "tenant-1", "job-1")
			if err != nil {
				t.Fatalf("GetExportFile error = %v", err)
			}
			if file.FileID != "file-1" {
				t.Fatalf("file = %+v", file)
			}
		})
	}
}

func TestGetExportFilePropagatesNotFound(t *testing.T) {
	stub := &verifyRepositoryStub{exportFileErr: ErrNotFound}
	service := newVerifyService(t, stub)
	if _, err := service.GetExportFile(context.Background(), "tenant-1", "job-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetExportFile error = %v, want ErrNotFound", err)
	}
}

// 分批导出必须把调用方的批量收敛到 [1, ExportEventBatchSize]，避免单个请求退化为
// 无界查询；合法的小批量保留给测试与低内存实例。
func TestListExportEventsBoundsBatchSize(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "default for zero", limit: 0, want: ExportEventBatchSize},
		{name: "default for negative", limit: -5, want: ExportEventBatchSize},
		{name: "clamp oversized", limit: ExportEventBatchSize + 1, want: ExportEventBatchSize},
		{name: "keep small batch", limit: 25, want: 25},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &recordingExportRepository{verifyRepositoryStub: verifyRepositoryStub{}}
			service := newVerifyService(t, stub)
			if _, err := service.ListExportEvents(context.Background(), "tenant-1", domain.ExportQuery{}, domain.ExportCursor{}, test.limit); err != nil {
				t.Fatalf("ListExportEvents error = %v", err)
			}
			if stub.gotLimit != test.want {
				t.Fatalf("repository limit = %d, want %d", stub.gotLimit, test.want)
			}
		})
	}
}

func TestListExportEventsRejectsBlankTenant(t *testing.T) {
	service := newVerifyService(t, &verifyRepositoryStub{})
	if _, err := service.ListExportEvents(context.Background(), "  ", domain.ExportQuery{}, domain.ExportCursor{}, 10); !errors.Is(err, ErrValidation) {
		t.Fatalf("ListExportEvents error = %v, want ErrValidation", err)
	}
}

// recordingExportRepository 记录服务层传给仓储的批量，用于断言收敛规则。
type recordingExportRepository struct {
	verifyRepositoryStub
	gotLimit int
}

func (r *recordingExportRepository) ListExportEvents(_ context.Context, _ string, _ domain.ExportQuery, _ domain.ExportCursor, limit int) (domain.ExportEventPage, error) {
	r.gotLimit = limit
	return domain.ExportEventPage{}, nil
}
