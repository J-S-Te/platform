package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filemigrations "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestReserveUploadConcurrentOnMySQL 在真实 MySQL 唯一键和事务语义下验证上传会话仲裁。
// 默认跳过；CI 或本地集成测试通过 FILE_GATEWAY_TEST_DSN 指向专用空数据库后运行。
func TestReserveUploadConcurrentOnMySQL(t *testing.T) {
	dsn := os.Getenv("FILE_GATEWAY_TEST_DSN")
	if dsn == "" {
		t.Skip("FILE_GATEWAY_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := filemigrations.Run(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	// FILE_GATEWAY_TEST_DSN 必须指向专用测试库；按外键依赖顺序清理，保证该集成测试可重放。
	for _, table := range []string{"file_upload_session", "file_binding", "file_version", "file_object", "async_job"} {
		if err := database.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 16
	hash := sha256.Sum256([]byte("complete upload request"))
	start := make(chan struct{})
	createdIDs := make(chan string, callers)
	errorsFound := make(chan error, callers)
	var createdCount atomic.Int32
	var waitGroup sync.WaitGroup
	for index := range callers {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			file, version := mysqlReservationFixture(index, "request-concurrent", hash[:])
			_, created, reserveErr := repository.ReserveUpload(context.Background(), file, version)
			if reserveErr != nil {
				errorsFound <- reserveErr
				return
			}
			if created {
				createdCount.Add(1)
				createdIDs <- file.ID
			}
		}(index)
	}
	close(start)
	waitGroup.Wait()
	close(createdIDs)
	close(errorsFound)

	for reserveErr := range errorsFound {
		if !errors.Is(reserveErr, application.ErrConflict) {
			t.Fatalf("concurrent reserve error = %v, want conflict", reserveErr)
		}
	}
	if createdCount.Load() != 1 {
		t.Fatalf("created reservations = %d, want 1", createdCount.Load())
	}
	winnerID := <-createdIDs
	for _, table := range []string{"file_upload_session", "file_object", "file_version"} {
		var count int64
		if err := database.Table(table).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d, want 1", table, count)
		}
	}

	digest := sha256.Sum256([]byte("stored content"))
	if err := repository.MarkValidating(context.Background(), "01TENANT000000000000000000", winnerID, 14, digest[:], time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkReady(context.Background(), "01TENANT000000000000000000", winnerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	replayFile, replayVersion := mysqlReservationFixture(99, "request-concurrent", hash[:])
	replayed, created, err := repository.ReserveUpload(context.Background(), replayFile, replayVersion)
	if err != nil || created || replayed.File.ID != winnerID {
		t.Fatalf("ready replay = %#v, created=%v, err=%v", replayed, created, err)
	}

	changedHash := sha256.Sum256([]byte("changed request"))
	_, changedVersion := mysqlReservationFixture(100, "request-concurrent", changedHash[:])
	if _, _, err := repository.ReserveUpload(context.Background(), replayFile, changedVersion); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("changed hash error = %v, want conflict", err)
	}

	failedFile, failedVersion := mysqlReservationFixture(101, "request-failed", hash[:])
	if _, created, err := repository.ReserveUpload(context.Background(), failedFile, failedVersion); err != nil || !created {
		t.Fatalf("reserve failed-session fixture: created=%v err=%v", created, err)
	}
	if err := repository.MarkFailed(context.Background(), failedFile.TenantID, failedFile.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	retryFile, retryVersion := mysqlReservationFixture(102, "request-failed", hash[:])
	if _, _, err := repository.ReserveUpload(context.Background(), retryFile, retryVersion); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("failed session replay error = %v, want conflict", err)
	}
}

func mysqlReservationFixture(index int, requestID string, requestHash []byte) (domain.File, domain.FileVersion) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	fileID := fmt.Sprintf("%026d", index+1)
	versionID := fmt.Sprintf("%026d", index+1001)
	file := domain.File{
		ID: fileID, TenantID: "01TENANT000000000000000000", ApplicationID: "01APP000000000000000000000",
		OriginalName: "evidence.txt", FileExtension: ".txt", MediaType: "text/plain", Classification: "CONFIDENTIAL",
		OwnerUserID: "01USER00000000000000000000", CurrentVersionNo: 1, CurrentVersionID: versionID,
		Status: domain.FileStatusPendingUpload, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	version := domain.FileVersion{
		ID: versionID, FileID: fileID, VersionNo: 1,
		StorageRelativePath: fmt.Sprintf("tenant/app/2026/08/%s/%s.bin", fileID, versionID),
		MediaType:           "text/plain", OriginalName: "evidence.txt", UploaderUserID: file.OwnerUserID,
		UploadRequestID: requestID, UploadRequestHash: append([]byte(nil), requestHash...),
		Status: domain.FileVersionStatusPendingUpload, CreatedAt: now,
	}
	return file, version
}
