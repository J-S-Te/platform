package worker

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

const archiveMediaType = "application/x-ndjson+gzip"

// ArchiveWriter 把审计事件写成 gzip 压缩 NDJSON。数据库只持久化相对路径和 SHA-256，
// 不把宿主机绝对路径暴露为归档元数据；底层目录若可能被不可信进程替换，仍需文件系统权限隔离。
type ArchiveWriter struct{ storageRoot string }

func NewArchiveWriter(storageRoot string) (*ArchiveWriter, error) {
	storageRoot = strings.TrimSpace(storageRoot)
	if storageRoot == "" {
		return nil, errors.New("audit archive storage root is required")
	}
	return &ArchiveWriter{storageRoot: storageRoot}, nil
}

func (writer *ArchiveWriter) WriteArchive(ctx context.Context, task domain.RetentionTask, events []domain.Event, now time.Time) (domain.Archive, error) {
	if strings.TrimSpace(task.ArchiveID) == "" || strings.TrimSpace(task.TenantID) == "" || strings.TrimSpace(task.ApplicationID) == "" {
		return domain.Archive{}, errors.New("archive task identity is required")
	}
	relativePath := filepath.Join("audit-archive", task.TenantID, task.ApplicationID, now.UTC().Format("2006/01"), task.ArchiveID+".ndjson.gz")
	absolutePath, err := safeStoragePath(writer.storageRoot, relativePath)
	if err != nil {
		return domain.Archive{}, err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		return domain.Archive{}, fmt.Errorf("create audit archive directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolutePath), ".audit-archive-*.tmp")
	if err != nil {
		return domain.Archive{}, fmt.Errorf("create archive temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	completed := false
	defer func() {
		if !completed {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
		}
	}()
	hash := sha256.New()
	// 哈希计算的是最终 gzip 字节而非解压后的事件；校验时可直接验证归档文件是否被替换或损坏。
	compressed := gzip.NewWriter(io.MultiWriter(temporary, hash))
	encoder := json.NewEncoder(compressed)
	var occurredFrom, occurredTo time.Time
	for index, event := range events {
		if err := ctx.Err(); err != nil {
			return domain.Archive{}, err
		}
		if err := encoder.Encode(event); err != nil {
			return domain.Archive{}, fmt.Errorf("encode audit archive event: %w", err)
		}
		if index == 0 || event.OccurredAt.Before(occurredFrom) {
			occurredFrom = event.OccurredAt
		}
		if index == 0 || event.OccurredAt.After(occurredTo) {
			occurredTo = event.OccurredAt
		}
	}
	if err := compressed.Close(); err != nil {
		return domain.Archive{}, fmt.Errorf("close audit archive gzip: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return domain.Archive{}, fmt.Errorf("sync audit archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return domain.Archive{}, fmt.Errorf("close audit archive: %w", err)
	}
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
		return domain.Archive{}, fmt.Errorf("publish audit archive: %w", err)
	}
	// rename 发布完整文件后再降为只读；数据库清单只会引用已经完整落盘的归档。
	if err := os.Chmod(absolutePath, 0o440); err != nil {
		return domain.Archive{}, fmt.Errorf("set audit archive read-only: %w", err)
	}
	completed = true
	return domain.Archive{ArchiveID: task.ArchiveID, TenantID: task.TenantID, ApplicationID: task.ApplicationID, StorageRelativePath: filepath.ToSlash(relativePath), MediaType: archiveMediaType, SHA256: hash.Sum(nil), EventCount: uint64(len(events)), OccurredFrom: occurredFrom, OccurredTo: occurredTo, CreatedAt: now.UTC()}, nil
}
