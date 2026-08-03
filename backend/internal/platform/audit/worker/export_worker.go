// Package worker 执行由 MySQL 协调的审计导出任务，并将结果文件写入受控本地目录。
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

const (
	exportMediaType = "text/csv; charset=utf-8"
)

// ExportWorker 单次处理一个导出任务。任务所有权保存在 MySQL 而不是进程内，因此多个实例
// 可以并行轮询，无需依赖 Redis 或消息队列来避免重复领取。
type ExportWorker struct {
	service          *application.Service
	logger           *slog.Logger
	storageRoot      string
	workerID         string
	pollInterval     time.Duration
	staleLockTimeout time.Duration
}

func NewExportWorker(service *application.Service, logger *slog.Logger, storageRoot, workerID string, pollInterval, staleLockTimeout time.Duration) (*ExportWorker, error) {
	if service == nil || logger == nil {
		return nil, errors.New("audit export worker dependencies must not be nil")
	}
	if strings.TrimSpace(storageRoot) == "" || strings.TrimSpace(workerID) == "" || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("audit export worker configuration is invalid")
	}
	return &ExportWorker{service: service, logger: logger, storageRoot: filepath.Clean(storageRoot), workerID: workerID, pollInterval: pollInterval, staleLockTimeout: staleLockTimeout}, nil
}

// Run 持续轮询直到上下文取消。单个任务的错误会写回任务状态，不会终止后续任务处理。
func (w *ExportWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		w.ProcessOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne 至多领取一个到期任务；返回值仅表示是否成功领取，不表示导出成功。
// 这种语义便于确定性测试，也避免调度器把“已领取但已记录失败”再次当成立即重试信号。
func (w *ExportWorker) ProcessOne(ctx context.Context) bool {
	work, claimed, err := w.service.ClaimExportJob(ctx, w.workerID, time.Now().UTC().Add(-w.staleLockTimeout))
	if err != nil {
		w.logger.Error("claim audit export job", "error", err)
		return false
	}
	if !claimed {
		return false
	}
	if err := w.process(ctx, work); err != nil {
		w.logger.Error("process audit export job", "job_id", work.JobID, "error", err)
		message := err.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
		errorCode := "AUDIT_EXPORT_FAILED"
		if strings.Contains(message, "more than 10000 events") {
			errorCode = "AUDIT_EXPORT_TOO_LARGE"
			work.Attempts = work.MaxAttempts
		}
		if failErr := w.service.FailExportJob(ctx, work, errorCode, message, time.Now().UTC().Add(backoff(work.Attempts))); failErr != nil {
			w.logger.Error("mark audit export job failed", "job_id", work.JobID, "error", failErr)
		}
	}
	return true
}

func (w *ExportWorker) process(ctx context.Context, work domain.ExportWork) error {
	events, err := w.service.ListExportEvents(ctx, work.TenantID, work.Query)
	if err != nil {
		return fmt.Errorf("list audit events: %w", err)
	}
	file, absolutePath, err := w.writeCSV(work, events)
	if err != nil {
		return err
	}
	if err := w.service.CompleteExportJob(ctx, work, file); err != nil {
		// 文件先于数据库完成记录落盘；若元数据提交失败，删除孤儿文件，让过期租约可安全重试。
		_ = os.Remove(absolutePath)
		return fmt.Errorf("record audit export file: %w", err)
	}
	w.logger.Info("audit export completed", "job_id", work.JobID, "event_count", len(events))
	return nil
}

func (w *ExportWorker) writeCSV(work domain.ExportWork, events []domain.Event) (domain.ExportFile, string, error) {
	now := time.Now().UTC()
	fileID, err := ulid.New(now)
	if err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("generate export file ID: %w", err)
	}
	versionID, err := ulid.New(now.Add(time.Millisecond))
	if err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("generate export version ID: %w", err)
	}
	relativePath := filepath.Join(work.TenantID, work.ApplicationID, fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", now.Month()), fileID, versionID+".csv")
	absolutePath, err := safeStoragePath(w.storageRoot, relativePath)
	if err != nil {
		return domain.ExportFile{}, "", err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("create audit export directory: %w", err)
	}
	temporaryPath := absolutePath + ".tmp"
	output, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("create audit export file: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			_ = output.Close()
			_ = os.Remove(temporaryPath)
		}
	}()

	hash := sha256.New()
	// CSV 字节在写盘的同时进入哈希，避免二次读取大文件；摘要覆盖最终文件的精确内容。
	writer := csv.NewWriter(io.MultiWriter(output, hash))
	if err := writer.Write([]string{"event_id", "occurred_at", "application_code", "environment_code", "action", "result", "resource_type", "resource_id", "resource_name", "operator", "risk_level", "summary", "request_id", "trace_id", "correlation_id", "method", "path", "client_ip", "user_agent"}); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("write audit export header: %w", err)
	}
	for _, event := range events {
		if err := writer.Write([]string{event.EventID, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ApplicationCode, event.EnvironmentCode, event.Action, event.Result, event.ResourceType, event.ResourceID, event.ResourceName, event.OperatorDisplayName, event.RiskLevel, event.Summary, event.RequestID, event.TraceID, event.CorrelationID, event.Method, event.Path, event.ClientIP, event.UserAgent}); err != nil {
			return domain.ExportFile{}, "", fmt.Errorf("write audit export row: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("flush audit export CSV: %w", err)
	}
	if err := output.Sync(); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("sync audit export file: %w", err)
	}
	if err := output.Close(); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("close audit export file: %w", err)
	}
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("publish audit export file: %w", err)
	}
	// 同目录 rename 是发布边界：下载端只会看到完整文件，不会读到仍在生成的 CSV。
	info, err := os.Stat(absolutePath)
	if err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("stat audit export file: %w", err)
	}
	completed = true
	return domain.ExportFile{FileID: fileID, VersionID: versionID, StorageRelativePath: filepath.ToSlash(relativePath), OriginalName: "audit-export-" + work.JobID + ".csv", MediaType: exportMediaType, SizeBytes: uint64(info.Size()), SHA256: hash.Sum(nil), CreatedAt: now}, absolutePath, nil
}

func safeStoragePath(root, relativePath string) (string, error) {
	if filepath.IsAbs(relativePath) {
		return "", errors.New("audit export path must be relative")
	}
	absolutePath := filepath.Join(root, relativePath)
	resolvedRelative, err := filepath.Rel(root, absolutePath)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return "", errors.New("audit export path escapes storage root")
	}
	return absolutePath, nil
}

func backoff(attempt uint) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	return time.Second * time.Duration(1<<attempt)
}
