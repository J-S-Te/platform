// Package worker 执行由 MySQL 协调的审计导出任务，并将结果文件写入受控本地目录。
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

const (
	exportMediaType = "text/csv; charset=utf-8"

	// ExportRetentionDaysEnv 是审计导出产物保留期的唯一配置入口，单位是天。
	ExportRetentionDaysEnv = "AUDIT_EXPORT_RETENTION_DAYS"

	// defaultExportRetentionDays 是未配置或配置非法时 fail-closed 采用的保留期（30 天）。
	defaultExportRetentionDays = 30

	// maxExportRetentionDays 是保留期硬上限（3650 天）。超过上限按上限截断，避免一次
	// 误配把审计导出产物事实上永久留在磁盘上。
	maxExportRetentionDays = 3650

	// ExportRetentionPeriod 是审计导出产物的默认保留期：任务成功超过该时长后，worker 每轮
	// 开始时删除磁盘文件并把 file_version 标记为 DELETED；file_object/async_job 行保留。
	// 实际生效值可由 AUDIT_EXPORT_RETENTION_DAYS 覆盖，见 ParseExportRetention。
	ExportRetentionPeriod = defaultExportRetentionDays * 24 * time.Hour

	// exportCleanupMaxRounds 限定单轮清理的有界工作量：每轮最多处理
	// ExportArtifactCleanupBatchSize*exportCleanupMaxRounds 个产物，避免一次清理长期占用 worker。
	exportCleanupMaxRounds = 5
)

// ParseExportRetention 解析 AUDIT_EXPORT_RETENTION_DAYS 的原始值，返回生效天数与保留期。
// 未设置、空白、非整数或 <=0 一律 fail-closed 回落默认 30 天（宁可多留证据，不可提前
// 销毁）；超过硬上限 3650 天按上限截断。
func ParseExportRetention(raw string) (int, time.Duration) {
	days := defaultExportRetentionDays
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if parsed, err := strconv.Atoi(trimmed); err == nil && parsed > 0 {
			days = parsed
		}
	}
	if days > maxExportRetentionDays {
		days = maxExportRetentionDays
	}
	return days, time.Duration(days) * 24 * time.Hour
}

// ExportRetentionFromEnv 读取 AUDIT_EXPORT_RETENTION_DAYS 并解析为生效保留期。
// 返回值同时包含生效天数，供装配侧写启动日志，避免“配了但不知道是否生效”。
func ExportRetentionFromEnv() (int, time.Duration) {
	return ParseExportRetention(os.Getenv(ExportRetentionDaysEnv))
}

// exportHeader 是审计导出 CSV 的固定表头列；顺序是对外契约，不能随意调整。
var exportHeader = []string{"event_id", "occurred_at", "application_code", "environment_code", "action", "result", "resource_type", "resource_id", "resource_name", "operator", "risk_level", "summary", "request_id", "trace_id", "correlation_id", "method", "path", "client_ip", "user_agent", "actor_id", "actor_type", "reason_code", "business_id", "login_ip", "login_ip_source", "ingest_peer_ip", "ip_source", "received_at"}

// ExportWorker 单次处理一个导出任务。任务所有权保存在 MySQL 而不是进程内，因此多个实例
// 可以并行轮询，无需依赖 Redis 或消息队列来避免重复领取。
type ExportWorker struct {
	service          *application.Service
	logger           *slog.Logger
	storageRoot      string
	workerID         string
	pollInterval     time.Duration
	staleLockTimeout time.Duration
	// exportBatchSize 只用于测试注入小批量；生产固定使用 application.ExportEventBatchSize。
	exportBatchSize int
	// retention 是生效的产物保留期：由装配侧按 AUDIT_EXPORT_RETENTION_DAYS 注入，
	// 零值或负值回落 ExportRetentionPeriod（默认 30 天）。
	retention time.Duration
}

// NewExportWorker 组装一次性导出 worker 并校验配置。
// 返回值携带 MySQL 作业队列连接、导出目录、执行实例标识、轮询周期、租约超时与
// 产物保留期；pollInterval 或 staleLockTimeout 小于等于 0、存储根目录空或依赖为空会返回错误。
// exportRetention <= 0 表示采用默认保留期，由 retentionPeriod 统一回落。
func NewExportWorker(service *application.Service, logger *slog.Logger, storageRoot, workerID string, pollInterval, staleLockTimeout, exportRetention time.Duration) (*ExportWorker, error) {
	if service == nil || logger == nil {
		return nil, errors.New("audit export worker dependencies must not be nil")
	}
	if strings.TrimSpace(storageRoot) == "" || strings.TrimSpace(workerID) == "" || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("audit export worker configuration is invalid")
	}
	return &ExportWorker{service: service, logger: logger, storageRoot: filepath.Clean(storageRoot), workerID: workerID, pollInterval: pollInterval, staleLockTimeout: staleLockTimeout, retention: exportRetention}, nil
}

// StorageRoot 返回导出产物根目录（写入/清理的唯一位置），供装配测试断言它与下载
// handler 使用同一个根。绝不返回平台上传根。
func (w *ExportWorker) StorageRoot() string { return w.storageRoot }

// Run 持续轮询直到上下文取消。单个任务的错误会写回任务状态，不会终止后续任务处理。
func (w *ExportWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce 是单轮工作：先清理超过保留期的导出产物（失败只告警，绝不阻断导出），
// 再至多领取一个到期任务。返回值表示本轮是否领取到任务。
func (w *ExportWorker) RunOnce(ctx context.Context) bool {
	w.cleanupExpiredExports(ctx)
	return w.ProcessOne(ctx)
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
		if failErr := w.service.FailExportJob(ctx, work, "AUDIT_EXPORT_FAILED", message, time.Now().UTC().Add(backoff(work.Attempts))); failErr != nil {
			w.logger.Error("mark audit export job failed", "job_id", work.JobID, "error", failErr)
		}
	}
	return true
}

func (w *ExportWorker) process(ctx context.Context, work domain.ExportWork) error {
	file, absolutePath, eventCount, err := w.writeCSV(ctx, work)
	if err != nil {
		return err
	}
	if err := w.service.CompleteExportJob(ctx, work, file); err != nil {
		// 文件先于数据库完成记录落盘；若元数据提交失败，删除孤儿文件，让过期租约可安全重试。
		_ = os.Remove(absolutePath)
		return fmt.Errorf("record audit export file: %w", err)
	}
	w.logger.Info("audit export completed", "job_id", work.JobID, "event_count", eventCount)
	return nil
}

// writeCSV 按稳定排序分批读取事件并流式写入 CSV：内存占用只与单批大小相关，导出总量
// 不再构成失败条件。任一批次读取或写入失败都会中止并删除临时文件。
func (w *ExportWorker) writeCSV(ctx context.Context, work domain.ExportWork) (domain.ExportFile, string, int, error) {
	file, err := w.newExportCSV(work)
	if err != nil {
		return domain.ExportFile{}, "", 0, err
	}
	defer file.abort()

	eventCount := 0
	cursor := domain.ExportCursor{}
	for {
		page, err := w.service.ListExportEvents(ctx, work.TenantID, work.Query, cursor, w.batchSize())
		if err != nil {
			return domain.ExportFile{}, "", eventCount, fmt.Errorf("list audit events: %w", err)
		}
		if len(page.Events) == 0 {
			break
		}
		if err := file.writeEvents(page.Events); err != nil {
			return domain.ExportFile{}, "", eventCount, err
		}
		eventCount += len(page.Events)
		if page.NextCursor == nil {
			break
		}
		next := *page.NextCursor
		if next == cursor {
			return domain.ExportFile{}, "", eventCount, fmt.Errorf("audit export cursor did not advance after %d events", eventCount)
		}
		cursor = next
	}
	exportFile, err := file.publish()
	if err != nil {
		return domain.ExportFile{}, "", eventCount, err
	}
	return exportFile, file.absolutePath, eventCount, nil
}

// batchSize 返回本 worker 的单批行数；测试可注入更小的批量以覆盖多批次场景。
func (w *ExportWorker) batchSize() int {
	if w.exportBatchSize > 0 && w.exportBatchSize <= application.ExportEventBatchSize {
		return w.exportBatchSize
	}
	return application.ExportEventBatchSize
}

// retentionPeriod 返回产物保留期；测试可注入更短的保留期。
func (w *ExportWorker) retentionPeriod() time.Duration {
	if w.retention > 0 {
		return w.retention
	}
	return ExportRetentionPeriod
}

// cleanupExpiredExports 删除超过保留期的审计导出磁盘文件，并把对应 file_version 标记为
// DELETED。选择范围由仓储限定为 AUDIT_EXPORT 任务产物；路径必须落在 storageRoot 内。
// 任何清理失败都只记录告警，让下一轮重试，不影响导出任务领取与执行。
func (w *ExportWorker) cleanupExpiredExports(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-w.retentionPeriod())
	cleaned := 0
	for round := 0; round < exportCleanupMaxRounds; round++ {
		artifacts, err := w.service.ListExpiredExportArtifacts(ctx, cutoff, application.ExportArtifactCleanupBatchSize)
		if err != nil {
			w.logger.Warn("list expired audit export artifacts", "error", err)
			return
		}
		if len(artifacts) == 0 {
			break
		}
		deleted := make([]string, 0, len(artifacts))
		for _, artifact := range artifacts {
			absolutePath, err := safeStoragePath(w.storageRoot, artifact.StorageRelativePath)
			if err != nil {
				w.logger.Warn("skip expired audit export artifact with unsafe path", "file_version_id", artifact.FileVersionID, "error", err)
				continue
			}
			if err := os.Remove(absolutePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				w.logger.Warn("delete expired audit export file", "file_version_id", artifact.FileVersionID, "error", err)
				continue
			}
			deleted = append(deleted, artifact.FileVersionID)
		}
		if err := w.service.MarkExportArtifactsDeleted(ctx, deleted); err != nil {
			w.logger.Warn("mark expired audit export artifacts deleted", "count", len(deleted), "error", err)
			return
		}
		cleaned += len(deleted)
		if len(deleted) < len(artifacts) {
			// 有文件删除失败：其版本保持可用状态，留待下一轮重试，本轮不再热循环。
			break
		}
		if len(artifacts) < application.ExportArtifactCleanupBatchSize {
			break
		}
	}
	if cleaned > 0 {
		w.logger.Info("expired audit export artifacts cleaned", "count", cleaned, "retention", w.retentionPeriod().String())
	}
}

// exportCSV 是流式写出的导出文件：临时文件 → 逐批追加行 → Sync → 同目录 rename 发布。
// 发布前下载端看不到半成品；发布后只有完整文件可见。
type exportCSV struct {
	work          domain.ExportWork
	fileID        string
	versionID     string
	createdAt     time.Time
	relativePath  string
	absolutePath  string
	temporaryPath string
	output        *os.File
	hash          hash.Hash
	writer        *csv.Writer
	published     bool
}

// newExportCSV 创建临时文件并写入 UTF-8 BOM 与表头。BOM 进入 SHA-256 摘要，保证下载
// 校验覆盖最终文件精确字节。
func (w *ExportWorker) newExportCSV(work domain.ExportWork) (*exportCSV, error) {
	now := time.Now().UTC()
	fileID, err := ulid.New(now)
	if err != nil {
		return nil, fmt.Errorf("generate export file ID: %w", err)
	}
	versionID, err := ulid.New(now.Add(time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("generate export version ID: %w", err)
	}
	relativePath := filepath.Join(work.TenantID, work.ApplicationID, fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", now.Month()), fileID, versionID+".csv")
	absolutePath, err := safeStoragePath(w.storageRoot, relativePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o750); err != nil {
		return nil, fmt.Errorf("create audit export directory: %w", err)
	}
	temporaryPath := absolutePath + ".tmp"
	output, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create audit export file: %w", err)
	}
	file := &exportCSV{work: work, relativePath: relativePath, absolutePath: absolutePath, temporaryPath: temporaryPath, output: output, hash: sha256.New(), createdAt: now, fileID: fileID, versionID: versionID}
	// CSV 字节在写盘的同时进入哈希，避免二次读取大文件；摘要覆盖最终文件的精确内容。
	// Excel 中文环境需要 UTF-8 BOM 才会按 UTF-8 解析；BOM 也进入摘要，保证下载校验一致。
	sink := io.MultiWriter(output, file.hash)
	if _, err := sink.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		file.abort()
		return nil, fmt.Errorf("write audit export BOM: %w", err)
	}
	file.writer = csv.NewWriter(sink)
	if err := file.writer.Write(exportHeader); err != nil {
		file.abort()
		return nil, fmt.Errorf("write audit export header: %w", err)
	}
	return file, nil
}

// writeEvents 追加一批事件行。安全审查 SEC-B6：所有数据单元格统一做公式注入转义——
// 攻击者可控字段（User-Agent/资源名/路径等）若以 = + - @ 或前导 tab/CR 开头，审计员用
// Excel/WPS 打开导出文件时会被解释为公式并可能执行。
func (file *exportCSV) writeEvents(events []domain.Event) error {
	for _, event := range events {
		receivedAt := ""
		if !event.ReceivedAt.IsZero() {
			receivedAt = event.ReceivedAt.UTC().Format(time.RFC3339Nano)
		}
		row := []string{event.EventID, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ApplicationCode, event.EnvironmentCode, event.Action, event.Result, event.ResourceType, event.ResourceID, event.ResourceName, event.OperatorDisplayName, event.RiskLevel, event.Summary, event.RequestID, event.TraceID, event.CorrelationID, event.Method, event.Path, event.ClientIP, event.UserAgent, event.ActorID, event.ActorType, event.ReasonCode, event.BusinessID, event.LoginIP, event.LoginIPSource, event.IngestPeerIP, event.IPSource, receivedAt}
		for index := range row {
			row[index] = escapeCSVFormulaCell(row[index])
		}
		if err := file.writer.Write(row); err != nil {
			return fmt.Errorf("write audit export row: %w", err)
		}
	}
	return nil
}

// publish 刷盘并原子发布文件，返回可供任务完成记录的文件元数据。
func (file *exportCSV) publish() (domain.ExportFile, error) {
	file.writer.Flush()
	if err := file.writer.Error(); err != nil {
		return domain.ExportFile{}, fmt.Errorf("flush audit export CSV: %w", err)
	}
	if err := file.output.Sync(); err != nil {
		return domain.ExportFile{}, fmt.Errorf("sync audit export file: %w", err)
	}
	if err := file.output.Close(); err != nil {
		return domain.ExportFile{}, fmt.Errorf("close audit export file: %w", err)
	}
	if err := os.Rename(file.temporaryPath, file.absolutePath); err != nil {
		return domain.ExportFile{}, fmt.Errorf("publish audit export file: %w", err)
	}
	// 同目录 rename 是发布边界：下载端只会看到完整文件，不会读到仍在生成的 CSV。
	info, err := os.Stat(file.absolutePath)
	if err != nil {
		// 发布后无法确认大小属于异常路径：删除已重命名的文件，避免留下元数据未登记的孤儿。
		_ = os.Remove(file.absolutePath)
		return domain.ExportFile{}, fmt.Errorf("stat audit export file: %w", err)
	}
	file.published = true
	return domain.ExportFile{FileID: file.fileID, VersionID: file.versionID, StorageRelativePath: filepath.ToSlash(file.relativePath), OriginalName: "audit-export-" + file.work.JobID + ".csv", MediaType: exportMediaType, SizeBytes: uint64(info.Size()), SHA256: file.hash.Sum(nil), CreatedAt: file.createdAt}, nil
}

// abort 在未发布时关闭并删除临时文件；已发布文件由调用方在元数据提交失败时删除。
func (file *exportCSV) abort() {
	if file == nil || file.published {
		return
	}
	if file.output != nil {
		_ = file.output.Close()
	}
	_ = os.Remove(file.temporaryPath)
}

// escapeCSVFormulaCell 对公式注入载荷加前导单引号（OWASP CSV Injection 防御）：
// 以 =、+、-、@ 开头，或前导含 tab/CR（表格软件会先剥掉这些控制字符再判定公式）的
// 单元格一律加 ' 前缀，使其按字面文本渲染；普通值原样返回，不改变既有导出内容。
func escapeCSVFormulaCell(value string) string {
	if value == "" {
		return value
	}
	trimmed := strings.TrimLeft(value, "\t\r")
	if trimmed != value {
		// 前导 tab/CR 本身就是公式解析绕过面，按同样方式转义。
		return "'" + value
	}
	switch value[0] {
	case '=', '+', '-', '@':
		return "'" + value
	default:
		return value
	}
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
