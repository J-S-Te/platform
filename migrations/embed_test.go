package migrations_test

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
)

func TestEmbeddedMigrationsAreContiguousAndParseable(t *testing.T) {
	items, err := migration.Load(migrations.Files)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	const latestMigrationVersion = 90
	if len(items) == 0 || items[len(items)-1].Version != latestMigrationVersion {
		t.Fatalf("last migration version = %d, want %d", items[len(items)-1].Version, latestMigrationVersion)
	}
	for _, item := range items {
		if _, err := migration.SplitStatements(item.SQL); err != nil {
			t.Fatalf("parse migration %06d_%s: %v", item.Version, item.Name, err)
		}
	}
}
