package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filemigrations "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestResolveUploadRequestOnMySQL(t *testing.T) {
	dsn := os.Getenv("FILE_GATEWAY_TEST_DSN")
	if dsn == "" {
		t.Skip("FILE_GATEWAY_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := filemigrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := fmt.Sprintf("%026d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range []string{"file_upload_request_guard", "file_upload_session", "file_version", "file_object"} {
			query := "tenant_id = ?"
			if table == "file_version" {
				query = "file_id IN (SELECT id FROM file_object WHERE tenant_id = ?)"
			}
			if err := db.Exec("DELETE FROM "+table+" WHERE "+query, tenant).Error; err != nil {
				t.Error(err)
			}
		}
	})
	for index, status := range []string{"WRITING", "READY", "FAILED", "REJECTED"} {
		hash := sha256.Sum256([]byte(status))
		requestID := "resolve-" + tenant + "-" + status
		file, version := mysqlReservationFixture(index+9900, requestID, hash[:])
		file.TenantID = tenant
		file.ID = fmt.Sprintf("%024s%02d", tenant[:24], index)
		file.CurrentVersionID = fmt.Sprintf("%024s%02d", tenant[:24], index+10)
		version.ID, version.FileID = file.CurrentVersionID, file.ID
		if _, created, err := r.ReserveUpload(ctx, file, version); err != nil || !created {
			t.Fatalf("reserve created=%t err=%v", created, err)
		}
		if status == "FAILED" {
			err = r.MarkFailed(ctx, tenant, file.ID, time.Now().UTC())
		} else if status != "WRITING" {
			if err = r.MarkValidating(ctx, tenant, file.ID, 1, hash[:], time.Now().UTC()); err == nil {
				if status == "READY" {
					err = r.MarkReady(ctx, tenant, file.ID, time.Now().UTC())
				} else {
					err = r.MarkRejected(ctx, tenant, file.ID, time.Now().UTC())
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		id, actual, err := r.LookupUploadRequest(ctx, tenant, file.ApplicationID, requestID)
		if err != nil || id != file.ID || actual != status {
			t.Fatalf("resolve %s id=%s status=%s err=%v", status, id, actual, err)
		}
		for _, scope := range [][2]string{{"other-tenant", file.ApplicationID}, {tenant, "other-app"}} {
			if _, _, err := r.LookupUploadRequest(ctx, scope[0], scope[1], requestID); !errors.Is(err, application.ErrNotFound) {
				t.Fatalf("cross scope disclosed upload: %v", err)
			}
		}
	}
}
