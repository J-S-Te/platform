package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setHandoverBaselineEnv 设置通过 Validate 所需的最小环境，避免测试依赖宿主 .env。
func setHandoverBaselineEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_NAME", "basic-platform")
	t.Setenv("APP_HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("APP_PUBLIC_BASE_URL", "https://platform.example.com")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "https://platform.example.com")
	t.Setenv("MYSQL_HOST", "127.0.0.1")
	t.Setenv("MYSQL_PORT", "3306")
	t.Setenv("MYSQL_DATABASE", "basic_platform")
	t.Setenv("MYSQL_USERNAME", "basic_platform")
	t.Setenv("AUTH_JWT_ISSUER", "basic-platform")
	t.Setenv("AUTH_JWT_AUDIENCE", "basic-platform-console")
	t.Setenv("AUTH_APPLICATION_JWT_AUDIENCE", "basic-platform-integration")
	t.Setenv("OIDC_ISSUER", "https://platform.example.com")
	t.Setenv("AUTH_SESSION_COOKIE_SECURE", "true")
	t.Setenv("AUTH_SESSION_COOKIE_SAME_SITE", "Lax")
	t.Setenv("AUTH_SESSION_TTL", "8h")
	t.Setenv("LOG_DIRECTORY", "/var/log/basic-platform")
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("ASYNC_WORKER_ID", "basic-platform-worker-01")
	t.Setenv("AUDIT_APPLICATION_CODE", "platform")
	t.Setenv("AUDIT_ENVIRONMENT_CODE", "prod")
}

func TestHandoverConfigurationIsDisabledByDefault(t *testing.T) {
	setHandoverBaselineEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Handover.Enabled || len(cfg.Handover.SystemURLs) != 0 {
		t.Fatalf("handover must stay disabled by default: %+v", cfg.Handover)
	}
}

func TestHandoverConfigurationParsesSystemsAndTimeout(t *testing.T) {
	setHandoverBaselineEnv(t)
	t.Setenv("HANDOVER_ENABLED", "true")
	t.Setenv("HANDOVER_ENVIRONMENT", "prod")
	t.Setenv("HANDOVER_MACHINE_TOKEN", "machine-token-value")
	t.Setenv("HANDOVER_SYSTEM_URLS", " Contract=https://contract.internal , crm=https://crm.internal ,")
	t.Setenv("HANDOVER_TIMEOUT", "15s")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Handover.Enabled || cfg.Handover.Environment != "prod" || cfg.Handover.Timeout != 15*time.Second {
		t.Fatalf("handover section = %+v", cfg.Handover)
	}
	if len(cfg.Handover.SystemURLs) != 2 || cfg.Handover.SystemURLs["contract"] != "https://contract.internal" || cfg.Handover.SystemURLs["crm"] != "https://crm.internal" {
		t.Fatalf("system urls = %v", cfg.Handover.SystemURLs)
	}
}

func TestHandoverConfigurationFailsClosedOnMissingCredentialOrSystems(t *testing.T) {
	cases := map[string]struct {
		env        map[string]string
		wantPhrase string
	}{
		"missing-token": {
			env:        map[string]string{"HANDOVER_ENABLED": "true", "HANDOVER_SYSTEM_URLS": "contract=https://contract.internal"},
			wantPhrase: "HANDOVER_MACHINE_TOKEN",
		},
		"missing-systems": {
			env:        map[string]string{"HANDOVER_ENABLED": "true", "HANDOVER_MACHINE_TOKEN": "machine-token-value"},
			wantPhrase: "HANDOVER_SYSTEM_URLS",
		},
		"malformed-entry-only": {
			env:        map[string]string{"HANDOVER_ENABLED": "true", "HANDOVER_MACHINE_TOKEN": "machine-token-value", "HANDOVER_SYSTEM_URLS": "contract,="},
			wantPhrase: "HANDOVER_SYSTEM_URLS",
		},
		"credentials-in-url": {
			env:        map[string]string{"HANDOVER_ENABLED": "true", "HANDOVER_MACHINE_TOKEN": "machine-token-value", "HANDOVER_SYSTEM_URLS": "contract=https://user:secret@contract.internal"},
			wantPhrase: "without credentials",
		},
		"non-http-scheme": {
			env:        map[string]string{"HANDOVER_ENABLED": "true", "HANDOVER_MACHINE_TOKEN": "machine-token-value", "HANDOVER_SYSTEM_URLS": "contract=ftp://contract.internal"},
			wantPhrase: "absolute http(s) URL",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			setHandoverBaselineEnv(t)
			for key, value := range testCase.env {
				t.Setenv(key, value)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), testCase.wantPhrase) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, testCase.wantPhrase)
			}
		})
	}
}
