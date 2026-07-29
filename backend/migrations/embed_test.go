package migrations_test

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/migration"
	"github.com/J-S-Te/Basic-Platform/backend/migrations"
)

func TestEmbeddedMigrationsAreContiguousAndParseable(t *testing.T) {
	items, err := migration.Load(migrations.Files)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(items) == 0 || items[len(items)-1].Version != 56 {
		t.Fatalf("last migration version = %d, want 56", items[len(items)-1].Version)
	}
	for _, item := range items {
		if _, err := migration.SplitStatements(item.SQL); err != nil {
			t.Fatalf("parse migration %06d_%s: %v", item.Version, item.Name, err)
		}
	}
}
