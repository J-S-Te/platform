package migration_test

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
)

// 迁移集合本身必须始终可加载且版本唯一有序：整体迁移测试（如许可证隔离库用例）依赖
// migration.Load 推导"应当应用的版本数"，集合损坏时它们会以难以定位的方式失败。
func TestEmbeddedMigrationSetLoadsWithUniqueOrderedVersions(t *testing.T) {
	items, err := migration.Load(migrations.Files)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("embedded migration set is empty")
	}
	seen := make(map[uint64]string, len(items))
	previous := uint64(0)
	for _, item := range items {
		if item.Version == 0 {
			t.Fatalf("migration %q has version 0", item.Name)
		}
		if existing, duplicated := seen[item.Version]; duplicated {
			t.Fatalf("duplicate migration version %d: %s and %s", item.Version, existing, item.Name)
		}
		seen[item.Version] = item.Name
		if item.Version < previous {
			t.Fatalf("migration versions are not ascending: %d after %d", item.Version, previous)
		}
		previous = item.Version
	}
}
