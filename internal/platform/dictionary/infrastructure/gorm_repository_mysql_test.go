package infrastructure

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This test requires an empty disposable schema, never a business database.
func TestDictionaryAggregateReadbackOnIsolatedMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_DICTIONARY_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_DICTIONARY_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated dictionary database failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var tables int64
	if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("dictionary integration test requires an empty disposable database, found %d tables", tables)
	}
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	const tenantID = "01J00000000000000000000000"
	const dictionaryID = "01M4J000000000000000000001"
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	created, err := repository.CreateDictionary(ctx, application.DictionaryCreateInput{
		TenantID: tenantID, Code: "projection_regression", Name: "聚合回读测试", Description: "完整字段与租户隔离",
		Status: domain.StatusActive,
	}, dictionaryID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateItem(ctx, application.ItemCreateInput{
		TenantID: tenantID, DictionaryID: dictionaryID, Code: "first", Label: "第一项", Value: "first", Status: domain.StatusActive,
	}, "01M4J000000000000000000002", now); err != nil {
		t.Fatal(err)
	}
	assertDictionary := func(t *testing.T, got domain.Dictionary) {
		t.Helper()
		if got.ID != created.ID || got.TenantID != tenantID || got.Code != created.Code || got.Name != created.Name ||
			got.Description != created.Description || got.Status != domain.StatusActive || got.Version != 1 ||
			got.ItemCount != 1 || !got.UpdatedAt.Equal(now) {
			t.Fatalf("persisted dictionary aggregate mismatch: %#v", got)
		}
	}
	got, err := repository.GetDictionary(ctx, tenantID, dictionaryID)
	if err != nil {
		t.Fatal(err)
	}
	assertDictionary(t, got)
	page, err := repository.ListDictionaries(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 10, Keyword: created.Code, Status: "ACTIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filtered aggregate page count=%d len=%d", page.Total, len(page.Items))
	}
	assertDictionary(t, page.Items[0])
	if _, err := repository.GetDictionary(ctx, "01M4J000000000000000000099", dictionaryID); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("foreign tenant read must be not found, got %v", err)
	}
	foreign, err := repository.ListDictionaries(ctx, "01M4J000000000000000000099", application.PageRequest{Page: 1, PageSize: 10})
	if err != nil || foreign.Total != 0 || len(foreign.Items) != 0 {
		t.Fatalf("foreign tenant list leaked dictionaries: count=%d err=%v", foreign.Total, err)
	}
}
