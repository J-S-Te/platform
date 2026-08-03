// Package bootstrap 负责把基础设施依赖装配成可独立启动的进程。
package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/infrastructure"
	auditworker "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/worker"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/observability"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	"gorm.io/gorm"
)

// ProcessRunner is implemented by each background loop composed into the worker process.
type ProcessRunner interface {
	Run(context.Context)
}

// concurrentRunner 让互不依赖的后台循环共享同一取消信号；关闭时等待全部循环完成清理再退出进程。
type concurrentRunner struct {
	runners []ProcessRunner
}

func (runner *concurrentRunner) Run(ctx context.Context) {
	var group sync.WaitGroup
	for _, process := range runner.runners {
		if process == nil {
			continue
		}
		group.Add(1)
		go func(current ProcessRunner) {
			defer group.Done()
			current.Run(ctx)
		}(process)
	}
	group.Wait()
}

// Worker is the dependency container for local asynchronous job processing.
type Worker struct {
	Runner ProcessRunner
	Logger *slog.Logger

	database *gorm.DB
	logFile  io.Closer
}

// NewWorker 装配审计导出与留存清理任务。Worker 不执行 AutoMigrate，数据库变更只能由独立
// migrate 命令完成，避免扩容或重启后台任务时意外竞争 DDL。
func NewWorker(cfg config.Config) (*Worker, error) {
	if err := os.MkdirAll(cfg.FileStorageRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create file storage root: %w", err)
	}

	logger, logFile, err := observability.NewLogger(cfg.Logging, cfg.AppName, cfg.Environment)
	if err != nil {
		return nil, err
	}

	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}

	repository, err := auditinfrastructure.NewRepository(db)
	if err != nil {
		// 装配任一步失败都按创建顺序逆向释放资源，不能留下占用日志文件或连接池的半成品进程。
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	service, err := auditapplication.NewService(repository, ulid.Generator{}, auditapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	exportRunner, err := auditworker.NewExportWorker(service, logger, cfg.FileStorageRoot, cfg.Worker.ID, cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	archiveWriter, err := auditworker.NewArchiveWriter(cfg.FileStorageRoot)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	retentionService, err := auditapplication.NewRetentionService(
		repository,
		archiveWriter,
		ulid.Generator{},
		auditapplication.SystemClock{},
		&governanceAuditAdapter{service: service, config: cfg.Audit},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	retentionRunner, err := auditworker.NewRetentionWorker(retentionService, logger, cfg.Worker.ID+"-retention", cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	// 两类任务共享数据库但使用独立租约；其中一个退出不应静默停止另一个，统一由上层 context 收敛。
	runner := &concurrentRunner{runners: []ProcessRunner{exportRunner, retentionRunner}}
	return &Worker{Runner: runner, Logger: logger, database: db, logFile: logFile}, nil
}

// Close releases resources held by the worker process.
func (worker *Worker) Close() {
	if worker == nil {
		return
	}
	if err := database.Close(worker.database); err != nil {
		worker.Logger.Error("close worker database", "error", err)
	}
	if worker.logFile != nil {
		if err := worker.logFile.Close(); err != nil {
			worker.Logger.Error("close worker log file", "error", err)
		}
	}
}
