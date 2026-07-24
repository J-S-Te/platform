// Package worker runs MySQL-backed audit export jobs and stores generated CSV files locally.
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

// ExportWorker processes one audit export at a time. The lock itself lives in MySQL; this process
// is therefore safe to run in more than one instance without Redis or a message queue.
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

// Run polls MySQL until context cancellation. Errors on one job are persisted and do not stop
// later jobs from being processed.
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

// ProcessOne claims and processes at most one due export job. It returns true only when a job was
// claimed, which makes it useful for deterministic worker tests and controlled command execution.
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
		if failErr := w.service.FailExportJob(ctx, work, "AUDIT_EXPORT_FAILED", message, time.Now().UTC().Add(backoff(work.Attempts))); failErr != nil {
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
	writer := csv.NewWriter(io.MultiWriter(output, hash))
	if err := writer.Write([]string{"event_id", "occurred_at", "application_code", "environment_code", "action", "result", "resource_type", "resource_id", "resource_name", "operator", "risk_level", "summary"}); err != nil {
		return domain.ExportFile{}, "", fmt.Errorf("write audit export header: %w", err)
	}
	for _, event := range events {
		if err := writer.Write([]string{event.EventID, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ApplicationCode, event.EnvironmentCode, event.Action, event.Result, event.ResourceType, event.ResourceID, event.ResourceName, event.OperatorDisplayName, event.RiskLevel, event.Summary}); err != nil {
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
