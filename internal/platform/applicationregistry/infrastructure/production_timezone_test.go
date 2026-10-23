package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProductionComposePreservesTimezoneOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, ComposeFile: filepath.Join(root, "docker-compose.yml")}}
	path := filepath.Join(root, "runtime", "compose.timezone.yaml")
	args, _ := target.composeCommand("config", "--services")
	if containsString(args, path) {
		t.Fatal("missing override included")
	}
	if err := os.WriteFile(path, []byte(`{"services":{"project-api":{"environment":{"TZ":"Asia/Shanghai"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ = target.composeCommand("up", "--no-deps", "project-api")
	if !containsString(args, path) {
		t.Fatal("controlled update dropped timezone override")
	}
	for i, arg := range args {
		if arg == path && (i == 0 || args[i-1] != "--file") {
			t.Fatal("override not a Compose file argument")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target.config.ComposeFile, path); err != nil {
		t.Fatal(err)
	}
	args, _ = target.composeCommand("config", "--services")
	if containsString(args, path) {
		t.Fatal("symbolic link must not be consumed")
	}
}
