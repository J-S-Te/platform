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

// handoverEnabledEnv 是启用交接所需的最小键集合；用例只覆盖单键删除/改动。
func handoverEnabledEnv() map[string]string {
	return map[string]string{
		"HANDOVER_ENABLED":                "true",
		"HANDOVER_ENVIRONMENT":            "prod",
		"HANDOVER_SYSTEM_URLS":            "contract=https://contract.internal",
		"HANDOVER_MACHINE_CLIENT_ID":      "platform-handover",
		"HANDOVER_MACHINE_APPLICATION_ID": "platform",
		"HANDOVER_MACHINE_ENVIRONMENT_ID": "prod",
		"HANDOVER_MACHINE_SCOPE":          "handover.machine",
	}
}

func setHandoverEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	setHandoverBaselineEnv(t)
	for key, value := range overrides {
		t.Setenv(key, value)
	}
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
	// 停用态也必须带完整签发标识：平台自签 application JWT，不再依赖静态机器令牌。
	if cfg.Handover.MachineClientID != "platform-handover" || cfg.Handover.MachineApplicationID != "platform" ||
		cfg.Handover.MachineEnvironmentID != "prod" || cfg.Handover.MachineScope != "handover.machine" ||
		cfg.Handover.Environment != "prod" || cfg.Handover.Timeout != 10*time.Second {
		t.Fatalf("handover defaults = %+v", cfg.Handover)
	}
}

func TestHandoverConfigurationParsesApplicationTokenIdentity(t *testing.T) {
	env := handoverEnabledEnv()
	env["HANDOVER_SYSTEM_URLS"] = " Contract=https://contract.internal , crm=https://crm.internal ,"
	env["HANDOVER_TIMEOUT"] = "15s"
	env["HANDOVER_MACHINE_CLIENT_ID"] = "platform-handover-prod"
	env["HANDOVER_MACHINE_APPLICATION_ID"] = "platform-prod"
	env["HANDOVER_MACHINE_ENVIRONMENT_ID"] = "prod-eu"
	env["HANDOVER_MACHINE_SCOPE"] = "handover.machine handover.read"
	setHandoverEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Handover.Enabled || cfg.Handover.Environment != "prod" || cfg.Handover.Timeout != 15*time.Second {
		t.Fatalf("handover section = %+v", cfg.Handover)
	}
	if cfg.Handover.MachineClientID != "platform-handover-prod" || cfg.Handover.MachineApplicationID != "platform-prod" ||
		cfg.Handover.MachineEnvironmentID != "prod-eu" || cfg.Handover.MachineScope != "handover.machine handover.read" {
		t.Fatalf("handover machine identity = %+v", cfg.Handover)
	}
	if len(cfg.Handover.SystemURLs) != 2 || cfg.Handover.SystemURLs["contract"] != "https://contract.internal" || cfg.Handover.SystemURLs["crm"] != "https://crm.internal" {
		t.Fatalf("system urls = %v", cfg.Handover.SystemURLs)
	}
}

// TestHandoverConfigurationNoLongerRequiresStaticMachineToken 钉住 BIZ-XWIRE-04 的协议变更：
// 平台自行铸造 application JWT，历史 HANDOVER_MACHINE_TOKEN 不再是必填项。
func TestHandoverConfigurationNoLongerRequiresStaticMachineToken(t *testing.T) {
	env := handoverEnabledEnv()
	env["HANDOVER_MACHINE_TOKEN"] = ""
	setHandoverEnv(t, env)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("static machine token must no longer be required: %v", err)
	}
	if !cfg.Handover.Enabled {
		t.Fatal("handover must stay enabled")
	}
}

func TestHandoverConfigurationFailsClosedOnMissingApplicationTokenIdentity(t *testing.T) {
	cases := map[string]struct {
		key        string
		wantPhrase string
	}{
		"missing-client-id":      {key: "HANDOVER_MACHINE_CLIENT_ID", wantPhrase: "HANDOVER_MACHINE_CLIENT_ID"},
		"missing-application-id": {key: "HANDOVER_MACHINE_APPLICATION_ID", wantPhrase: "HANDOVER_MACHINE_APPLICATION_ID"},
		"missing-environment-id": {key: "HANDOVER_MACHINE_ENVIRONMENT_ID", wantPhrase: "HANDOVER_MACHINE_ENVIRONMENT_ID"},
		"missing-scope":          {key: "HANDOVER_MACHINE_SCOPE", wantPhrase: "HANDOVER_MACHINE_SCOPE"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := handoverEnabledEnv()
			env[testCase.key] = ""
			setHandoverEnv(t, env)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), testCase.wantPhrase) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, testCase.wantPhrase)
			}
		})
	}
}

func TestHandoverConfigurationFailsClosedOnMissingOrMalformedSystems(t *testing.T) {
	cases := map[string]struct {
		systemURLs string
		wantPhrase string
	}{
		"missing-systems":      {systemURLs: "", wantPhrase: "HANDOVER_SYSTEM_URLS"},
		"malformed-entry-only": {systemURLs: "contract,=", wantPhrase: "HANDOVER_SYSTEM_URLS"},
		"credentials-in-url":   {systemURLs: "contract=https://user:secret@contract.internal", wantPhrase: "without credentials"},
		"non-http-scheme":      {systemURLs: "contract=ftp://contract.internal", wantPhrase: "absolute http(s) URL"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := handoverEnabledEnv()
			env["HANDOVER_SYSTEM_URLS"] = testCase.systemURLs
			setHandoverEnv(t, env)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), testCase.wantPhrase) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, testCase.wantPhrase)
			}
		})
	}
}
