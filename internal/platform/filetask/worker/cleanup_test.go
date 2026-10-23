package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

type cleanupRepo struct {
	application.FileRepository
	deleted bool
}

func (*cleanupRepo) ListRecoveryCandidates(context.Context, string, time.Time, int) ([]domain.StoredFile, error) {
	return nil, nil
}
func (r *cleanupRepo) ClaimExpiredUnbound(_ context.Context, tenant string, _, _ time.Time) (domain.StoredFile, bool, error) {
	if r.deleted {
		return domain.StoredFile{}, false, nil
	}
	return domain.StoredFile{File: domain.File{ID: "old-temp", TenantID: tenant, Status: "DELETING", Version: 2}, Version: domain.FileVersion{StorageRelativePath: "old-temp"}}, true, nil
}
func (r *cleanupRepo) MarkDeleted(context.Context, string, string, uint64, time.Time) error {
	r.deleted = true
	return nil
}
func (*cleanupRepo) ListStaleDeleting(context.Context, string, time.Time, int) ([]domain.StoredFile, error) {
	return nil, nil
}

type cleanupStore struct {
	application.LocalStore
	removed bool
}

func (s *cleanupStore) Remove(string) error                   { s.removed = true; return nil }
func (*cleanupStore) CleanupTemporary(time.Time) (int, error) { return 0, nil }

type cleanupIDs struct{}

func (cleanupIDs) New(time.Time) (string, error) { return "id", nil }

type cleanupTenants struct{}

func (cleanupTenants) ListTenantIDs(context.Context) ([]string, error) {
	return []string{"tenant-1"}, nil
}

func TestRunnerSchedulesExpiredUnboundCleanup(t *testing.T) {
	repo, store := &cleanupRepo{}, &cleanupStore{}
	files, err := application.NewFileService(repo, store, cleanupIDs{}, application.SystemClock{}, application.DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(files, cleanupTenants{}, time.Minute, 15*time.Minute, 100, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runner.runOnce(context.Background())
	if !repo.deleted || !store.removed {
		t.Fatalf("scheduled maintenance did not remove eligible temporary file: deleted=%v removed=%v", repo.deleted, store.removed)
	}
}

func TestRunnerCleanupCanBeDisabledAndHonoursInterval(t *testing.T) {
	repo, store := &cleanupRepo{}, &cleanupStore{}
	files, err := application.NewFileService(repo, store, cleanupIDs{}, application.SystemClock{}, application.DefaultUploadPolicy())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(files, cleanupTenants{}, time.Minute, 15*time.Minute, 100, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ConfigureCleanup(false, time.Hour, 24*time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	runner.runOnce(context.Background())
	if repo.deleted || store.removed {
		t.Fatal("disabled cleanup removed bytes")
	}
	if err := runner.ConfigureCleanup(true, time.Hour, 24*time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	runner.now = func() time.Time { return now }
	runner.runOnce(context.Background())
	if !repo.deleted {
		t.Fatal("enabled cleanup did not run")
	}
	repo.deleted = false
	store.removed = false
	runner.runOnce(context.Background())
	if repo.deleted {
		t.Fatal("cleanup repeated before interval")
	}
	now = now.Add(time.Hour)
	runner.runOnce(context.Background())
	if !repo.deleted {
		t.Fatal("cleanup did not resume at interval")
	}
	if err := runner.ConfigureCleanup(true, time.Second, time.Minute, 501); err == nil {
		t.Fatal("unsafe cleanup settings accepted")
	}
}
