package migrations_test

import (
	"fmt"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
)

func TestEmbeddedMigrationsAreContiguousAndParseable(t *testing.T) {
	items, err := migration.Load(migrations.Files)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	const latestMigrationVersion = 110
	if len(items) == 0 || items[len(items)-1].Version != latestMigrationVersion {
		t.Fatalf("last migration version = %d, want %d", items[len(items)-1].Version, latestMigrationVersion)
	}
	for _, item := range items {
		if _, err := migration.SplitStatements(item.SQL); err != nil {
			t.Fatalf("parse migration %06d_%s: %v", item.Version, item.Name, err)
		}
		if item.Version == 107 {
			const appliedChecksum = "4a7af29819cf1f00b9b54ac04d3d28d57bb7a06eb8b938d3d11d82071eeaddcf"
			if actual := fmt.Sprintf("%x", item.Checksum); actual != appliedChecksum {
				t.Fatalf("migration 107 is already applied and must remain immutable: checksum=%s want=%s", actual, appliedChecksum)
			}
		}
	}
}
