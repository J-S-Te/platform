// Command file-gateway runs the isolated file metadata and binary service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	fileapp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	fileinfra "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filehttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
	filemigrations "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/migrations"
	fileworker "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/worker"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	if err := run(); err != nil {
		slog.Error("file gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	address := getenv("FILE_GATEWAY_HTTP_ADDRESS", ":8086")
	dsn := os.Getenv("FILE_GATEWAY_DATABASE_DSN")
	storageBackend := strings.ToLower(getenv("FILE_GATEWAY_STORAGE_BACKEND", "local"))
	root := os.Getenv("FILE_GATEWAY_STORAGE_ROOT")
	issuer, audience, publicKey := os.Getenv("FILE_GATEWAY_TOKEN_ISSUER"), os.Getenv("FILE_GATEWAY_TOKEN_AUDIENCE"), os.Getenv("FILE_GATEWAY_TOKEN_PUBLIC_KEY_PATH")
	if dsn == "" || issuer == "" || audience == "" || publicKey == "" {
		return errors.New("FILE_GATEWAY_DATABASE_DSN and token verifier settings are required")
	}
	database, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{TranslateError: true, SkipDefaultTransaction: true})
	if err != nil {
		return err
	}
	if err := filemigrations.Run(context.Background(), database); err != nil {
		return fmt.Errorf("run file gateway migrations: %w", err)
	}
	repository, err := fileinfra.NewGORMRepository(database)
	if err != nil {
		return err
	}
	var store fileapp.LocalStore
	var localStore *fileinfra.LocalStore
	if storageBackend != "local" {
		return fmt.Errorf("unsupported FILE_GATEWAY_STORAGE_BACKEND %q", storageBackend)
	}
	localStore, err = fileinfra.NewLocalStoreWithCapacityLimit(root, os.Getenv("FILE_GATEWAY_TEMP_ROOT"), os.Getenv("FILE_GATEWAY_QUARANTINE_ROOT"), uint64(intEnv("FILE_GATEWAY_CAPACITY_REJECT_PERCENT", 90)))
	store = localStore
	if err != nil {
		return fmt.Errorf("configure %s file storage: %w", storageBackend, err)
	}
	ready := func(ctx context.Context) error {
		sqlDatabase, databaseErr := database.DB()
		if databaseErr != nil {
			return databaseErr
		}
		if databaseErr = sqlDatabase.PingContext(ctx); databaseErr != nil {
			return databaseErr
		}
		if storage, ok := store.(interface{ Ready(context.Context) error }); ok {
			return storage.Ready(ctx)
		}
		return errors.New("file storage does not expose readiness")
	}
	startupContext, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	if err = ready(startupContext); err != nil {
		cancelStartup()
		return fmt.Errorf("file gateway dependency readiness failed: %w", err)
	}
	cancelStartup()
	files, err := fileapp.NewFileService(repository, store, ulid.Generator{}, fileapp.SystemClock{}, fileapp.DefaultUploadPolicy())
	if err != nil {
		return err
	}
	jobs, err := fileapp.NewJobService(repository, ulid.Generator{}, fileapp.SystemClock{})
	if err != nil {
		return err
	}
	handler, err := filehttp.NewHandler(files, jobs, slog.Default())
	if err != nil {
		return err
	}
	idGenerator := ulid.Generator{}
	v2Handler, err := filehttp.NewUploadV2Handler(database, files, idGenerator.New)
	if err != nil {
		return err
	}
	verifier, err := security.LoadApplicationJWTVerifier(issuer, audience, publicKey)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runner, err := fileworker.NewRunner(files, tenantSource{database}, durationEnv("FILE_GATEWAY_RECONCILIATION_INTERVAL", time.Minute), durationEnv("FILE_GATEWAY_RECONCILIATION_STALE_AFTER", 15*time.Minute), intEnv("FILE_GATEWAY_RECONCILIATION_BATCH_SIZE", 100), slog.Default())
	if err != nil {
		return err
	}
	go runner.Run(ctx)
	if localStore != nil {
		go monitorLocalCapacity(ctx, localStore, slog.Default())
	}
	server := &http.Server{Addr: address, Handler: routes(handler, v2Handler, tokenMiddleware{verifier}, ready, database, localStore), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func monitorLocalCapacity(ctx context.Context, store interface{ UsagePercent() (uint64, error) }, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		usage, err := store.UsagePercent()
		if err != nil {
			logger.Error("file gateway capacity check failed", "error", err)
		} else if usage >= 90 {
			logger.Error("file gateway storage critical", "used_percent", usage)
		} else if usage >= 80 {
			logger.Warn("file gateway storage high", "used_percent", usage)
		} else if usage >= 70 {
			logger.Warn("file gateway storage warning", "used_percent", usage)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func routes(handler *filehttp.Handler, v2 *filehttp.UploadV2Handler, middleware tokenMiddleware, ready func(context.Context) error, database *gorm.DB, capacity interface{ UsagePercent() (uint64, error) }) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		if ready == nil || ready(ctx) != nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /metrics", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		used := uint64(0)
		collectionError := 0
		if capacity == nil {
			collectionError = 1
		} else if value, usageErr := capacity.UsagePercent(); usageErr != nil {
			collectionError = 1
		} else {
			used = value
		}
		fmt.Fprintf(writer, "# TYPE file_gateway_storage_used_percent gauge\nfile_gateway_storage_used_percent %d\n", used)
		fmt.Fprintln(writer, "# TYPE file_gateway_storage_warning_threshold_percent gauge\nfile_gateway_storage_warning_threshold_percent 70")
		fmt.Fprintln(writer, "# TYPE file_gateway_storage_high_threshold_percent gauge\nfile_gateway_storage_high_threshold_percent 80")
		fmt.Fprintf(writer, "# TYPE file_gateway_metric_collection_errors gauge\nfile_gateway_metric_collection_errors %d\n", collectionError)
		if database != nil {
			if sqlDB, dbErr := database.DB(); dbErr == nil {
				stats := sqlDB.Stats()
				fmt.Fprintf(writer, "# TYPE file_gateway_database_open_connections gauge\nfile_gateway_database_open_connections %d\n", stats.OpenConnections)
				fmt.Fprintf(writer, "# TYPE file_gateway_database_in_use_connections gauge\nfile_gateway_database_in_use_connections %d\n", stats.InUse)
			}
		}
	})
	mux.Handle("POST /api/v1/files", middleware.wrap(http.HandlerFunc(handler.Upload), "platform:file:upload"))
	mux.Handle("GET /api/v1/files/{file_id}/content", middleware.wrap(http.HandlerFunc(handler.Download), "platform:file:download"))
	mux.Handle("POST /api/v1/files/{file_id}/bindings", middleware.wrap(http.HandlerFunc(handler.BindFile), "platform:file:bind"))
	mux.Handle("DELETE /api/v1/files/{file_id}/bindings/{binding_id}", middleware.wrap(http.HandlerFunc(handler.UnbindFile), "platform:file:bind"))
	mux.Handle("POST /api/v1/files/cleanup", middleware.wrap(http.HandlerFunc(handler.CleanupFiles), "platform:file:cleanup"))
	mux.Handle("POST /api/v1/files/reconcile", middleware.wrap(http.HandlerFunc(handler.ReconcileFiles), "platform:file:cleanup"))
	mux.Handle("POST /api/v2/upload-sessions", middleware.wrap(http.HandlerFunc(v2.CreateSession), "platform:file:upload"))
	mux.Handle("POST /api/v2/upload-sessions/{upload_id}/tickets", middleware.wrap(http.HandlerFunc(v2.IssueUploadTicket), "platform:file:upload"))
	mux.HandleFunc("PUT /api/v2/upload-sessions/{upload_id}/content", v2.UploadContent)
	mux.Handle("POST /api/v2/upload-sessions/{upload_id}/complete", middleware.wrap(http.HandlerFunc(v2.Complete), "platform:file:upload"))
	mux.Handle("GET /api/v2/upload-sessions/{upload_id}", middleware.wrap(http.HandlerFunc(v2.GetSession), "platform:file:upload"))
	mux.Handle("GET /api/v2/files/{file_id}", middleware.wrap(http.HandlerFunc(v2.GetSession), "platform:file:download"))
	mux.Handle("POST /api/v2/files/{file_id}/download-tickets", middleware.wrap(http.HandlerFunc(v2.IssueDownloadTicket), "platform:file:download"))
	mux.HandleFunc("GET /api/v2/files/{file_id}/content", v2.DownloadContent)
	mux.Handle("DELETE /api/v2/files/{file_id}/bindings/{binding_id}", middleware.wrap(http.HandlerFunc(v2.UnbindFile), "platform:file:bind"))
	return mux
}

type tenantSource struct{ database *gorm.DB }

func (source tenantSource) ListTenantIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := source.database.WithContext(ctx).Table("file_object").Distinct("tenant_id").Where("tenant_id <> ''").Pluck("tenant_id", &ids).Error
	return ids, err
}
func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(os.Getenv(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}
func intEnv(key string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}
