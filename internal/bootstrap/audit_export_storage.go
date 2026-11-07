package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	audithttp "github.com/J-S-Te/Basic-Platform/internal/platform/audit/interfaces/http"
	auditworker "github.com/J-S-Te/Basic-Platform/internal/platform/audit/worker"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// auditExportStorage 把审计导出产物的新根与兼容只读根绑定在一起。
//
// 导出 worker（写/清理）与 API 下载 handler（读取）必须都从 auditExportStorageFrom 取根：
// 两边各自读一个配置字段时，任何一次改字段都可能让根悄悄分叉，worker 刚写出的产物在
// 下载端变成 404。LegacyRoot 只用于一个 release 的只读回退，写入与清理永不使用它。
type auditExportStorage struct {
	Root       string
	LegacyRoot string
}

// auditExportStorageFrom 解析审计导出根目录并校验它与平台上传根不同。
// 空值（未经 config.Load 的 Config）失败关闭：宁可启动失败，也不能退回与上传共用根。
func auditExportStorageFrom(cfg config.Config) (auditExportStorage, error) {
	root := filepath.Clean(strings.TrimSpace(cfg.AuditExportStorageRoot))
	if root == "" || root == "." {
		return auditExportStorage{}, fmt.Errorf("AUDIT_EXPORT_STORAGE_ROOT is required; default is <FILE_STORAGE_ROOT>/audit-exports")
	}
	legacyRoot := filepath.Clean(strings.TrimSpace(cfg.FileStorageRoot))
	if root == legacyRoot {
		return auditExportStorage{}, fmt.Errorf("AUDIT_EXPORT_STORAGE_ROOT must not equal FILE_STORAGE_ROOT")
	}
	return auditExportStorage{Root: root, LegacyRoot: legacyRoot}, nil
}

// newAuditExportWorker 用共享的导出根构造导出 worker；保留期由调用方按
// AUDIT_EXPORT_RETENTION_DAYS 注入。
func newAuditExportWorker(cfg config.Config, service *auditapplication.Service, logger *slog.Logger, retention time.Duration) (*auditworker.ExportWorker, error) {
	storage, err := auditExportStorageFrom(cfg)
	if err != nil {
		return nil, err
	}
	return auditworker.NewExportWorker(service, logger, storage.Root, cfg.Worker.ID, cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout, retention)
}

// newAuditExportHandler 用共享的导出根构造下载 handler，并保留旧上传根的只读回退，
// 让上一 release 写在 FILE_STORAGE_ROOT 下的产物在兼容窗口内仍可下载。
func newAuditExportHandler(cfg config.Config, service *auditapplication.Service, logger *slog.Logger) (*audithttp.Handler, error) {
	storage, err := auditExportStorageFrom(cfg)
	if err != nil {
		return nil, err
	}
	return audithttp.NewHandler(service, logger, storage.Root, storage.LegacyRoot)
}

// ensureAuditExportStorageRoot 解析并创建审计导出根（含默认子目录）。
// 它只创建导出目录，不会创建、清空或以任何方式触碰平台上传根。
func ensureAuditExportStorageRoot(cfg config.Config) (auditExportStorage, error) {
	storage, err := auditExportStorageFrom(cfg)
	if err != nil {
		return auditExportStorage{}, err
	}
	if err := os.MkdirAll(storage.Root, 0o750); err != nil {
		return auditExportStorage{}, fmt.Errorf("create audit export storage root: %w", err)
	}
	return storage, nil
}
