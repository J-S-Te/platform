package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 统一审计方案 §3.4：归档被替换或缺失时，清理必须失败关闭。
func TestVerifyArchiveDetectsTampering(t *testing.T) {
	root := t.TempDir()
	writer, err := NewArchiveWriter(root)
	if err != nil {
		t.Fatalf("NewArchiveWriter: %v", err)
	}
	task := domain.RetentionTask{TenantID: "tenant-1", ApplicationID: "app-1", ArchiveID: "archive-1"}
	archive, err := writer.WriteArchive(t.Context(), task, []domain.Event{{EventID: "evt-1", OccurredAt: time.Now().UTC()}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := writer.VerifyArchive(archive); err != nil {
		t.Fatalf("原始归档应校验通过：%v", err)
	}
	absolute := filepath.Join(root, archive.StorageRelativePath)
	// 归档文件是 0o600 只读发布，篡改测试先补写权限（模拟磁盘被替换后的现场）。
	if err := os.Chmod(absolute, 0o600); err != nil {
		t.Fatalf("chmod 归档：%v", err)
	}
	if err := os.WriteFile(absolute, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("篡改归档：%v", err)
	}
	if err := writer.VerifyArchive(archive); err == nil {
		t.Fatal("篡改后的归档必须校验失败")
	}
	if err := os.Remove(absolute); err != nil {
		t.Fatalf("删除归档：%v", err)
	}
	if err := writer.VerifyArchive(archive); err == nil {
		t.Fatal("缺失的归档必须校验失败")
	}
}
