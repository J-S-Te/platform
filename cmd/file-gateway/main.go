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
	switch storageBackend {
	case "local":
		store, err = fileinfra.NewLocalStore(root)
	case "s3":
		storageContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		store, err = fileinfra.NewS3ObjectStore(storageContext, fileinfra.S3ObjectStoreOptions{
			Bucket: os.Getenv("FILE_GATEWAY_S3_BUCKET"), Prefix: os.Getenv("FILE_GATEWAY_S3_PREFIX"),
			Endpoint: os.Getenv("FILE_GATEWAY_S3_ENDPOINT"), Region: getenv("FILE_GATEWAY_S3_REGION", "us-east-1"),
			AccessKeyID: os.Getenv("FILE_GATEWAY_S3_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("FILE_GATEWAY_S3_SECRET_ACCESS_KEY"),
			SessionToken: os.Getenv("FILE_GATEWAY_S3_SESSION_TOKEN"), UsePathStyle: boolEnv("FILE_GATEWAY_S3_USE_PATH_STYLE", false),
			MaxReadBytes: int64(intEnv("FILE_GATEWAY_S3_MAX_READ_BYTES", 100<<20)),
		})
	default:
		return fmt.Errorf("unsupported FILE_GATEWAY_STORAGE_BACKEND %q", storageBackend)
	}
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
	server := &http.Server{Addr: address, Handler: routes(handler, tokenMiddleware{verifier}, ready), ReadHeaderTimeout: 10 * time.Second}
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

func routes(handler *filehttp.Handler, middleware tokenMiddleware, ready func(context.Context) error) http.Handler {
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
	mux.Handle("POST /api/v1/files", middleware.wrap(http.HandlerFunc(handler.Upload), "platform:file:upload"))
	mux.Handle("GET /api/v1/files/{file_id}/content", middleware.wrap(http.HandlerFunc(handler.Download), "platform:file:download"))
	mux.Handle("POST /api/v1/files/{file_id}/bindings", middleware.wrap(http.HandlerFunc(handler.BindFile), "platform:file:bind"))
	mux.Handle("DELETE /api/v1/files/{file_id}/bindings/{binding_id}", middleware.wrap(http.HandlerFunc(handler.UnbindFile), "platform:file:bind"))
	mux.Handle("POST /api/v1/files/cleanup", middleware.wrap(http.HandlerFunc(handler.CleanupFiles), "platform:file:cleanup"))
	mux.Handle("POST /api/v1/files/reconcile", middleware.wrap(http.HandlerFunc(handler.ReconcileFiles), "platform:file:cleanup"))
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

func boolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
