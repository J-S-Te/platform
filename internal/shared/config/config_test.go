package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadUsesPublicBaseURLWhenOIDCIssuerIsBlank(t *testing.T) {
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
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("AUTH_SESSION_COOKIE_SECURE", "true")
	t.Setenv("AUTH_SESSION_COOKIE_SAME_SITE", "Lax")
	t.Setenv("AUTH_SESSION_TTL", "8h")
	t.Setenv("LOG_DIRECTORY", "/var/log/basic-platform")
	t.Setenv("FILE_STORAGE_ROOT", "/var/lib/basic-platform/uploads")
	t.Setenv("ASYNC_WORKER_ID", "basic-platform-worker-01")
	t.Setenv("AUDIT_APPLICATION_CODE", "platform")
	t.Setenv("AUDIT_ENVIRONMENT_CODE", "prod")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.Auth.OIDCIssuer, cfg.HTTP.PublicBaseURL; got != want {
		t.Fatalf("OIDC issuer = %q, want APP_PUBLIC_BASE_URL %q", got, want)
	}
	if got, want := cfg.Keycloak.OutageLoginPolicy, KeycloakOutagePolicyContinueExistingPlatformSessions; got != want {
		t.Fatalf("Keycloak outage policy = %q, want %q", got, want)
	}
}

func TestLoadRejectsUnsupportedKeycloakOutageLoginPolicy(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("KEYCLOAK_OUTAGE_LOGIN_POLICY", "fail_open")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want unsupported Keycloak outage policy error")
	}
}

func TestValueOrDefaultKeepsConfiguredValue(t *testing.T) {
	const configuredIssuer = "https://issuer.example.com"
	t.Setenv("OIDC_ISSUER", configuredIssuer)

	if got := valueOrDefault("OIDC_ISSUER", "https://platform.example.com"); got != configuredIssuer {
		t.Fatalf("valueOrDefault() = %q, want %q", got, configuredIssuer)
	}
}

func TestValidateRejectsInvalidTrustedProxy(t *testing.T) {
	cfg := Config{
		Environment: "development", AppName: "basic-platform",
		HTTP:     HTTPConfig{Addr: ":8080", PublicBaseURL: "http://localhost:8080", TrustedProxies: []string{"not-an-ip"}},
		MySQL:    MySQLConfig{Host: "127.0.0.1", Port: 3306, Database: "basic_platform", Username: "basic_platform"},
		Auth:     AuthConfig{JWTIssuer: "basic-platform", JWTAudience: "console", ApplicationJWTAudience: "application", OIDCIssuer: "http://localhost:8080", SessionCookieName: "bp_session", SessionCookieSameSite: "Lax", SessionTTL: 8 * time.Hour},
		Identity: IdentityConfig{},
		Logging:  LoggingConfig{Directory: "/tmp/logs"}, FileStorageRoot: "/tmp/uploads",
		Worker:      WorkerConfig{ID: "worker", PollInterval: time.Second, StaleLockTimeout: time.Minute},
		Audit:       AuditConfig{ApplicationCode: "platform", EnvironmentCode: "dev"},
		CORSOrigins: []string{"http://localhost:5173"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid trusted proxy error")
	}
}

func TestValidateAllowsExplicitHTTPDeploymentInProduction(t *testing.T) {
	cfg := Config{
		Environment: "production", AppName: "basic-platform",
		HTTP:    HTTPConfig{Addr: ":8080", PublicBaseURL: "http://platform.internal:8080"},
		MySQL:   MySQLConfig{Host: "127.0.0.1", Port: 3306, Database: "basic_platform", Username: "basic_platform"},
		Auth:    AuthConfig{JWTIssuer: "basic-platform", JWTAudience: "console", ApplicationJWTAudience: "application", OIDCIssuer: "http://platform.internal:8080", SessionCookieName: "bp_session", SessionCookieSameSite: "Lax", SessionTTL: 8 * time.Hour},
		Logging: LoggingConfig{Directory: "/tmp/logs"}, FileStorageRoot: "/tmp/uploads",
		Worker:      WorkerConfig{ID: "worker", PollInterval: time.Second, StaleLockTimeout: time.Minute},
		Audit:       AuditConfig{ApplicationCode: "platform", EnvironmentCode: "prod"},
		Keycloak:    KeycloakConfig{OutageLoginPolicy: KeycloakOutagePolicyContinueExistingPlatformSessions},
		CORSOrigins: []string{"http://platform.internal:8080"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want HTTP-compatible production configuration", err)
	}
}

func TestLoadReadsOAuthClientInsecureHTTPRedirectURIsSetting(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs {
		t.Fatal("OAuthClientAllowInsecureHTTPRedirectURIs = false, want true")
	}
}

func TestLoadDefaultsLegacyPlatformAccessTokenAccessCompatEnabled(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("APP_HTTP_ADDR", "127.0.0.1:8080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Auth.AllowLegacyPlatformAccessToken {
		t.Fatal("AllowLegacyPlatformAccessToken = false, want true")
	}
}

func TestLoadReadsLegacyPlatformAccessTokenCompatFlag(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("APP_HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("AUTH_LEGACY_PLATFORM_ACCESS_TOKEN_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.AllowLegacyPlatformAccessToken {
		t.Fatal("AllowLegacyPlatformAccessToken = true, want false")
	}
}

func TestLoadReadsKeycloakAdminClientCredentials(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_ID", "platform-admin")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "client-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.Keycloak.AdminClientID, "platform-admin"; got != want {
		t.Fatalf("AdminClientID = %q, want %q", got, want)
	}
	if got, want := cfg.Keycloak.AdminClientSecret, "client-secret"; got != want {
		t.Fatalf("AdminClientSecret = %q, want %q", got, want)
	}
}

func TestLoadReadsOptionalKeycloakHTTPSCutoverPolicy(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("KEYCLOAK_REQUIRE_HTTPS", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Keycloak.RequireHTTPS {
		t.Fatal("Keycloak.RequireHTTPS = false, want true")
	}
}

func TestValidateKeycloakManagementCredentials(t *testing.T) {
	base := Config{Keycloak: KeycloakConfig{Enabled: true, AdminURL: "http://keycloak:8080", PublicURL: "http://keycloak:8080", Realm: "acme", OutageLoginPolicy: KeycloakOutagePolicyContinueExistingPlatformSessions}}
	for name, credentials := range map[string]KeycloakConfig{
		"client credentials":   {AdminClientID: "platform-admin", AdminClientSecret: "client-secret"},
		"password credentials": {AdminUsername: "admin", AdminPassword: "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Keycloak.AdminClientID = credentials.AdminClientID
			cfg.Keycloak.AdminClientSecret = credentials.AdminClientSecret
			cfg.Keycloak.AdminUsername = credentials.AdminUsername
			cfg.Keycloak.AdminPassword = credentials.AdminPassword
			cfg.AppName = "basic-platform"
			cfg.HTTP = HTTPConfig{Addr: ":8080", PublicBaseURL: "http://platform.internal:8080"}
			cfg.MySQL = MySQLConfig{Host: "127.0.0.1", Port: 3306, Database: "basic_platform", Username: "basic_platform"}
			cfg.Auth = AuthConfig{JWTIssuer: "basic-platform", JWTAudience: "console", ApplicationJWTAudience: "application", OIDCIssuer: "http://platform.internal:8080", SessionCookieName: "bp_session", SessionCookieSameSite: "Lax", SessionTTL: time.Hour}
			cfg.Logging = LoggingConfig{Directory: "/tmp/logs"}
			cfg.FileStorageRoot = "/tmp/uploads"
			cfg.Worker = WorkerConfig{ID: "worker", PollInterval: time.Second, StaleLockTimeout: time.Minute}
			cfg.Audit = AuditConfig{ApplicationCode: "platform", EnvironmentCode: "dev"}
			cfg.CORSOrigins = []string{"http://platform.internal:8080"}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
