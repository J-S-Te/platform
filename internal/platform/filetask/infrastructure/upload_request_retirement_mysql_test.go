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
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filemigrations "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRetireUploadRequestFencesLateReserveAndBindingOnMySQL(t *testing.T) {
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
	app := "01APP000000000000000000000"
	t.Cleanup(func() {
		for _, table := range []string{"file_binding_release", "file_binding", "file_upload_request_guard", "file_upload_session", "file_version", "file_object"} {
			predicate := "tenant_id = ?"
			if table == "file_version" {
				predicate = "file_id IN (SELECT id FROM file_object WHERE tenant_id = ?)"
			}
			if err := db.Exec("DELETE FROM "+table+" WHERE "+predicate, tenant).Error; err != nil {
				t.Error(err)
			}
		}
	})
	fixture := func(index int, key string) (domain.File, domain.FileVersion) {
		hash := sha256.Sum256([]byte(key))
		f, v := mysqlReservationFixture(index+22000, key, hash[:])
		f.TenantID = tenant
		f.ID = fmt.Sprintf("%024s%02d", tenant[:24], index)
		f.CurrentVersionID = fmt.Sprintf("%024s%02d", tenant[:24], index+50)
		v.ID, v.FileID = f.CurrentVersionID, f.ID
		return f, v
	}

	// A request that has not reached Reserve cannot create a file afterward.
	key := "retired-before-reserve-" + tenant
	id, status, err := r.RetireUploadRequest(ctx, tenant, app, key)
	if err != nil || id != "" || status != "CANCELLED" {
		t.Fatalf("missing retirement id=%s status=%s err=%v", id, status, err)
	}
	f, v := fixture(0, key)
	if _, _, err := r.ReserveUpload(ctx, f, v); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("late reserve=%v", err)
	}
	if id, status, err := r.LookupUploadRequest(ctx, tenant, app, key); err != nil || id != "" || status != "CANCELLED" {
		t.Fatalf("retired resolution id=%s status=%s err=%v", id, status, err)
	}

	// Already reserved WRITING bytes may complete, but its old business tuple
	// stays released and even READY replay under the retired key is rejected.
	key = "retired-writing-" + tenant
	f, v = fixture(1, key)
	if _, _, err := r.ReserveUpload(ctx, f, v); err != nil {
		t.Fatal(err)
	}
	id, status, err = r.RetireUploadRequest(ctx, tenant, app, key)
	if err != nil || id != f.ID || status != "WRITING" {
		t.Fatalf("writing retirement id=%s status=%s err=%v", id, status, err)
	}
	binding := domain.FileBinding{ID: fmt.Sprintf("%024s80", tenant[:24]), TenantID: tenant, ApplicationID: app, FileID: f.ID, ResourceType: "contract_template_source", ResourceID: "source-old", BindingType: "TEMPLATE_SOURCE", CreatedBy: f.OwnerUserID, CreatedAt: time.Now().UTC()}
	if err := r.ReleaseBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("content"))
	if err := r.MarkValidating(ctx, tenant, f.ID, 7, hash[:], time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkReady(ctx, tenant, f.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReserveUpload(ctx, f, v); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("retired ready replay=%v", err)
	}
	if _, err := r.CreateBinding(ctx, binding); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("late binding=%v", err)
	}
	if id, status, err := r.RetireUploadRequest(ctx, tenant, app, key); err != nil || id != f.ID || status != "READY" {
		t.Fatalf("ready data was changed id=%s status=%s err=%v", id, status, err)
	}

	// Whichever side wins the shared request lock, retirement acknowledges the
	// exact committed file or permanently fences an absent one.
	for index := 2; index < 10; index++ {
		key = fmt.Sprintf("retire-race-%s-%d", tenant, index)
		f, v = fixture(index, key)
		start := make(chan struct{})
		reserveDone := make(chan error, 1)
		go func(file domain.File, version domain.FileVersion) {
			<-start
			_, _, err := r.ReserveUpload(ctx, file, version)
			reserveDone <- err
		}(f, v)
		close(start)
		id, status, err = r.RetireUploadRequest(ctx, tenant, app, key)
		if err != nil {
			t.Fatal(err)
		}
		reserveErr := <-reserveDone
		if reserveErr != nil && !errors.Is(reserveErr, application.ErrConflict) {
			t.Fatal(reserveErr)
		}
		if (id == "" && status != "CANCELLED") || (id != "" && (id != f.ID || status != "WRITING")) {
			t.Fatalf("race retirement id=%s status=%s", id, status)
		}
		if _, _, err := r.ReserveUpload(ctx, f, v); !errors.Is(err, application.ErrConflict) {
			t.Fatalf("race late reserve=%v", err)
		}
	}
}

func TestRetireUploadRequestAfterCleanupOnMySQL(t *testing.T) {
	db := cleanupTestDatabase(t)
	r, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)
	hash := sha256.Sum256([]byte("cleaned export"))
	f, v := mysqlReservationFixture(23000, fmt.Sprintf("retire-after-cleanup-%d", now.UnixNano()), hash[:])
	f.TenantID = fmt.Sprintf("%026d", now.UnixNano())
	f.RetentionClass = domain.RetentionClassTemporary
	f.RetentionUntil = &past
	f.CreatedAt = past
	if _, _, err := r.ReserveUpload(ctx, f, v); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkValidating(ctx, f.TenantID, f.ID, 14, hash[:], now); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkReady(ctx, f.TenantID, f.ID, now); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := r.ClaimExpiredUnbound(ctx, f.TenantID, now, now)
	if err != nil || !ok || claimed.File.ID != f.ID {
		t.Fatalf("cleanup claim id=%s found=%t err=%v", claimed.File.ID, ok, err)
	}
	if err := r.MarkDeleted(ctx, f.TenantID, f.ID, claimed.File.Version, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.LookupUploadRequest(ctx, f.TenantID, f.ApplicationID, v.UploadRequestID); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("removed bytes must not resolve as readable: %v", err)
	}
	fileID, status, err := r.RetireUploadRequest(ctx, f.TenantID, f.ApplicationID, v.UploadRequestID)
	if err != nil || fileID != f.ID || status != "READY" {
		t.Fatalf("cleaned request retirement file=%s original_session_status=%s err=%v", fileID, status, err)
	}
	var retired bool
	if err := db.Table("file_upload_request_guard").Select("retired").Where("tenant_id=? AND application_id=? AND request_id=?", f.TenantID, f.ApplicationID, v.UploadRequestID).Scan(&retired).Error; err != nil || !retired {
		t.Fatalf("cleanup rolled back request fence retired=%t err=%v", retired, err)
	}
	if _, _, err := r.ReserveUpload(ctx, f, v); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("cleaned request late reserve=%v", err)
	}
	var fileStatus, versionStatus string
	if err := db.Table("file_object").Select("status").Where("id=?", f.ID).Scan(&fileStatus).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("file_version").Select("status").Where("id=?", v.ID).Scan(&versionStatus).Error; err != nil {
		t.Fatal(err)
	}
	if fileStatus != "DELETED" || versionStatus != "REMOVED" {
		t.Fatalf("retirement restored cleaned bytes metadata: file=%s version=%s", fileStatus, versionStatus)
	}

	// An authenticated caller cannot return another application's file identity
	// through a corrupted session pointer, even though no bytes are being read.
	if err := db.Table("file_object").Where("id=?", f.ID).Update("application_id", "other-app").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RetireUploadRequest(ctx, f.TenantID, f.ApplicationID, v.UploadRequestID); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("corrupt file ownership accepted: %v", err)
	}
	if err := db.Table("file_object").Where("id=?", f.ID).Update("application_id", f.ApplicationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("file_upload_session").Where("tenant_id=? AND application_id=? AND request_id=?", f.TenantID, f.ApplicationID, v.UploadRequestID).Update("version_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RetireUploadRequest(ctx, f.TenantID, f.ApplicationID, v.UploadRequestID); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("missing version metadata was accepted: %v", err)
	}
	if err := db.Table("file_upload_session").Where("tenant_id=? AND application_id=? AND request_id=?", f.TenantID, f.ApplicationID, v.UploadRequestID).Update("version_id", v.ID).Error; err != nil {
		t.Fatal(err)
	}
}
