package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvKeepsProcessEnvironment(t *testing.T) {
	t.Setenv("APP_NAME", "from-process")

	const quotedValueKey = "DOTENV_TEST_QUOTED_VALUE"
	originalValue, wasSet := os.LookupEnv(quotedValueKey)
	if err := os.Unsetenv(quotedValueKey); err != nil {
		t.Fatalf("unset test environment value: %v", err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(quotedValueKey, originalValue)
			return
		}
		_ = os.Unsetenv(quotedValueKey)
	})

	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nAPP_NAME=from-file\nDOTENV_TEST_QUOTED_VALUE='local value'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write environment file: %v", err)
	}

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("load environment file: %v", err)
	}
	if got := os.Getenv("APP_NAME"); got != "from-process" {
		t.Fatalf("APP_NAME = %q, want process environment value", got)
	}
	if got := os.Getenv(quotedValueKey); got != "local value" {
		t.Fatalf("%s = %q, want parsed quoted value", quotedValueKey, got)
	}
}

func TestParseDotEnvLine(t *testing.T) {
	key, value, ok, err := parseDotEnvLine("export LOG_LEVEL=debug")
	if err != nil || !ok {
		t.Fatalf("parseDotEnvLine returned ok=%t err=%v", ok, err)
	}
	if key != "LOG_LEVEL" || value != "debug" {
		t.Fatalf("parseDotEnvLine = (%q, %q), want (LOG_LEVEL, debug)", key, value)
	}
}
