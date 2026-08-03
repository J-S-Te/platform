// Package config 从 .env 与进程环境加载启动配置，并在装配业务模块前统一执行安全校验。
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultEnvFile = ".env"

// Config 只包含进程启动所需配置；可在线修改的业务配置属于 configuration/settings 模块，
// 不能加入此结构后假装能够热更新。
type Config struct {
	Environment         string
	AppName             string
	Timezone            string
	HTTP                HTTPConfig
	MySQL               MySQLConfig
	Auth                AuthConfig
	Identity            IdentityConfig
	Logging             LoggingConfig
	Worker              WorkerConfig
	Audit               AuditConfig
	SubsystemOnboarding SubsystemOnboardingAutomationConfig
	FileStorageRoot     string
	CORSOrigins         []string
}

// HTTPConfig controls the API listener and public address.
type HTTPConfig struct {
	Addr           string
	PublicBaseURL  string
	TrustedProxies []string
}

// MySQLConfig contains the connection inputs used to build a MySQL DSN.
type MySQLConfig struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	Params   string
}

// AuthConfig contains the shared authentication settings used by later IAM work.
type AuthConfig struct {
	JWTIssuer              string
	JWTAudience            string
	ApplicationJWTAudience string
	OIDCIssuer             string
	JWTPrivateKeyPath      string
	JWTPublicKeyPath       string
	SessionCookieName      string
	SessionCookieSecure    bool
	SessionCookieSameSite  string
	SessionTTL             time.Duration
	// OAuthClientAllowInsecureHTTPRedirectURIs 仅放宽非回环 HTTP 回调登记；生产环境仍由整体配置校验约束 HTTPS。
	OAuthClientAllowInsecureHTTPRedirectURIs bool
}

// IdentityConfig controls encryption of IAM-sensitive fields and protected identity flows.
type IdentityConfig struct {
	MobileEncryptionKey string
	BootstrapToken      string
}

// LoggingConfig controls structured application logs.
type LoggingConfig struct {
	Level     string
	Format    string
	Directory string
}

// WorkerConfig controls polling for MySQL-backed asynchronous jobs.
type WorkerConfig struct {
	ID               string
	PollInterval     time.Duration
	StaleLockTimeout time.Duration
}

// AuditConfig identifies the platform application and environment used for server-generated
// lifecycle audit events. The referenced values must exist in application_registry and
// application_environment.
type AuditConfig struct {
	ApplicationCode string
	EnvironmentCode string
}

// SubsystemOnboardingAutomationConfig 控制一键接入的受信部署 Agent。API 只连接 Unix Socket，
// Docker Socket、宿主机文件和相邻项目目录权限必须留在隔离进程中。
type SubsystemOnboardingAutomationConfig struct {
	Enabled                 bool
	Mode                    string
	ProjectsRoot            string
	GatewayScriptPath       string
	GatewayIncludePath      string
	SocketPath              string
	ProductionDeployRoot    string
	ProductionRuntimeEnv    string
	ProductionContractEnv   string
	ProductionReleaseEnv    string
	ProductionComposeFile   string
	ProductionAllowedTenant string
	ProductionApplicationCode string
	ProductionApplicationName string
	ProductionDescription     string
	ProductionEnvironment     string
	ProductionUpstreamURL     string
	ProductionPathPrefix      string
	ProductionClientType      string
	PlatformComposeProject  string
	PlatformFrontendService string
	PlatformDockerNetwork   string
	DockerBinary            string
	Timeout                 time.Duration
}

// Load 显式设置 ENV_FILE 时只读取该文件；否则在当前目录及父目录寻找 .env，使 backend/ 下运行的
// 命令仍共享仓库根配置。宿主环境始终优先于文件，容器编排和密钥注入不会被本地文件覆盖。
func Load() (Config, error) {
	envFile := resolveEnvFile()
	if err := LoadDotEnv(envFile); err != nil {
		return Config{}, err
	}

	mysqlPort, err := integer("MYSQL_PORT", 3306)
	if err != nil {
		return Config{}, err
	}

	cookieSecure, err := boolean("AUTH_SESSION_COOKIE_SECURE", false)
	if err != nil {
		return Config{}, err
	}

	oauthClientAllowInsecureHTTPRedirectURIs, err := boolean("AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS", false)
	if err != nil {
		return Config{}, err
	}

	sessionTTL, err := duration("AUTH_SESSION_TTL", 8*time.Hour)
	if err != nil {
		return Config{}, err
	}
	workerPollInterval, err := duration("ASYNC_WORKER_POLL_INTERVAL", 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	workerStaleLockTimeout, err := duration("ASYNC_WORKER_STALE_LOCK_TIMEOUT", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	subsystemAutomationEnabled, err := boolean("SUBSYSTEM_ONBOARDING_AUTOMATION_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	subsystemAutomationTimeout, err := duration("SUBSYSTEM_ONBOARDING_TIMEOUT", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}

	publicBaseURL := value("APP_PUBLIC_BASE_URL", "http://localhost:8080")
	oidcIssuer := valueOrDefault("OIDC_ISSUER", publicBaseURL)

	cfg := Config{
		Environment: value("APP_ENV", "development"),
		AppName:     value("APP_NAME", "basic-platform"),
		Timezone:    value("APP_TIMEZONE", "Asia/Shanghai"),
		HTTP: HTTPConfig{
			Addr:           value("APP_HTTP_ADDR", ":8080"),
			PublicBaseURL:  publicBaseURL,
			TrustedProxies: commaSeparated(value("APP_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")),
		},
		MySQL: MySQLConfig{
			Host:     value("MYSQL_HOST", "127.0.0.1"),
			Port:     mysqlPort,
			Database: value("MYSQL_DATABASE", "basic_platform"),
			Username: value("MYSQL_USERNAME", "basic_platform"),
			Password: value("MYSQL_PASSWORD", ""),
			Params:   value("MYSQL_PARAMS", "charset=utf8mb4&parseTime=true&loc=UTC"),
		},
		Identity: IdentityConfig{
			MobileEncryptionKey: value("IAM_MOBILE_ENCRYPTION_KEY", ""),
			BootstrapToken:      strings.TrimSpace(value("IAM_BOOTSTRAP_TOKEN", "")),
		},
		Auth: AuthConfig{
			JWTIssuer:                                value("AUTH_JWT_ISSUER", "basic-platform"),
			JWTAudience:                              value("AUTH_JWT_AUDIENCE", "basic-platform-console"),
			ApplicationJWTAudience:                   value("AUTH_APPLICATION_JWT_AUDIENCE", "basic-platform-integration"),
			OIDCIssuer:                               strings.TrimRight(oidcIssuer, "/"),
			JWTPrivateKeyPath:                        resolveConfigPath(envFile, value("AUTH_JWT_PRIVATE_KEY_PATH", "")),
			JWTPublicKeyPath:                         resolveConfigPath(envFile, value("AUTH_JWT_PUBLIC_KEY_PATH", "")),
			SessionCookieName:                        value("AUTH_SESSION_COOKIE_NAME", "bp_session"),
			SessionCookieSecure:                      cookieSecure,
			SessionCookieSameSite:                    value("AUTH_SESSION_COOKIE_SAME_SITE", "Lax"),
			SessionTTL:                               sessionTTL,
			OAuthClientAllowInsecureHTTPRedirectURIs: oauthClientAllowInsecureHTTPRedirectURIs,
		},
		Logging: LoggingConfig{
			Level:     value("LOG_LEVEL", "info"),
			Format:    value("LOG_FORMAT", "json"),
			Directory: resolveConfigPath(envFile, value("LOG_DIRECTORY", filepath.Join("data", "logs"))),
		},
		Worker: WorkerConfig{
			ID:               value("ASYNC_WORKER_ID", "basic-platform-worker"),
			PollInterval:     workerPollInterval,
			StaleLockTimeout: workerStaleLockTimeout,
		},
		Audit: AuditConfig{
			ApplicationCode: value("AUDIT_APPLICATION_CODE", "platform"),
			EnvironmentCode: value("AUDIT_ENVIRONMENT_CODE", "dev"),
		},
		SubsystemOnboarding: SubsystemOnboardingAutomationConfig{
			Enabled:                 subsystemAutomationEnabled,
			Mode:                    strings.ToLower(value("SUBSYSTEM_ONBOARDING_MODE", "local")),
			ProjectsRoot:            value("SUBSYSTEM_PROJECTS_ROOT", ""),
			GatewayScriptPath:       value("SUBSYSTEM_GATEWAY_SCRIPT_PATH", ""),
			GatewayIncludePath:      value("SUBSYSTEM_GATEWAY_INCLUDE_PATH", ""),
			SocketPath:              value("SUBSYSTEM_PROVISIONING_SOCKET_PATH", "/run/basic-platform-provisioner/provisioner.sock"),
			ProductionDeployRoot:    value("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT", ""),
			ProductionRuntimeEnv:    value("SUBSYSTEM_PRODUCTION_RUNTIME_ENV_PATH", ""),
			ProductionContractEnv:   value("SUBSYSTEM_PRODUCTION_CONTRACT_ENV_PATH", ""),
			ProductionReleaseEnv:    value("SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH", ""),
			ProductionComposeFile:   value("SUBSYSTEM_PRODUCTION_COMPOSE_FILE", ""),
			ProductionAllowedTenant: value("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID", ""),
			ProductionApplicationCode: value("SUBSYSTEM_PRODUCTION_APPLICATION_CODE", ""),
			ProductionApplicationName: value("SUBSYSTEM_PRODUCTION_APPLICATION_NAME", ""),
			ProductionDescription:     value("SUBSYSTEM_PRODUCTION_APPLICATION_DESCRIPTION", ""),
			ProductionEnvironment:     strings.ToLower(value("SUBSYSTEM_PRODUCTION_ENVIRONMENT", "")),
			ProductionUpstreamURL:     value("SUBSYSTEM_PRODUCTION_UPSTREAM_URL", ""),
			ProductionPathPrefix:      value("SUBSYSTEM_PRODUCTION_PATH_PREFIX", ""),
			ProductionClientType:      strings.ToLower(value("SUBSYSTEM_PRODUCTION_CLIENT_TYPE", "")),
			PlatformComposeProject:  value("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-local"),
			PlatformFrontendService: value("SUBSYSTEM_PLATFORM_FRONTEND_SERVICE", "frontend"),
			PlatformDockerNetwork:   value("SUBSYSTEM_PLATFORM_DOCKER_NETWORK", "basic-platform-local_default"),
			DockerBinary:            value("SUBSYSTEM_DOCKER_BINARY", "docker"),
			Timeout:                 subsystemAutomationTimeout,
		},
		FileStorageRoot: resolveConfigPath(envFile, value("FILE_STORAGE_ROOT", filepath.Join("data", "uploads"))),
		CORSOrigins:     commaSeparated(value("APP_CORS_ALLOWED_ORIGINS", "http://localhost:5173")),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate 在创建网络监听和数据库连接前拒绝不安全配置。生产模式额外强制 HTTPS 与 Secure Cookie；
// CORS 使用凭据时禁止通配源；部署自动化只有在所选模式所需路径完整时才允许启用。
func (cfg Config) Validate() error {
	if cfg.AppName == "" {
		return fmt.Errorf("APP_NAME must not be empty")
	}
	if cfg.HTTP.Addr == "" {
		return fmt.Errorf("APP_HTTP_ADDR must not be empty")
	}
	for _, proxy := range cfg.HTTP.TrustedProxies {
		if !validTrustedProxy(proxy) {
			return fmt.Errorf("APP_TRUSTED_PROXIES contains invalid IP address or CIDR %q", proxy)
		}
	}
	publicBaseURL, err := url.ParseRequestURI(cfg.HTTP.PublicBaseURL)
	if err != nil || publicBaseURL.Scheme == "" || publicBaseURL.Host == "" || publicBaseURL.User != nil || publicBaseURL.RawQuery != "" || publicBaseURL.Fragment != "" {
		return fmt.Errorf("APP_PUBLIC_BASE_URL must be an absolute origin URL without credentials, query or fragment")
	}
	if cfg.MySQL.Host == "" || cfg.MySQL.Database == "" || cfg.MySQL.Username == "" {
		return fmt.Errorf("MYSQL_HOST, MYSQL_DATABASE and MYSQL_USERNAME must not be empty")
	}
	if cfg.MySQL.Port < 1 || cfg.MySQL.Port > 65535 {
		return fmt.Errorf("MYSQL_PORT must be between 1 and 65535")
	}
	if cfg.Auth.JWTIssuer == "" || cfg.Auth.JWTAudience == "" || cfg.Auth.ApplicationJWTAudience == "" {
		return fmt.Errorf("AUTH_JWT_ISSUER, AUTH_JWT_AUDIENCE and AUTH_APPLICATION_JWT_AUDIENCE must not be empty")
	}
	oidcIssuer, err := url.ParseRequestURI(cfg.Auth.OIDCIssuer)
	if err != nil || oidcIssuer.Scheme == "" || oidcIssuer.Host == "" || oidcIssuer.RawQuery != "" || oidcIssuer.Fragment != "" {
		return fmt.Errorf("OIDC_ISSUER must be an absolute origin URL without query or fragment")
	}
	if strings.TrimSpace(cfg.Identity.BootstrapToken) != "" && len(strings.TrimSpace(cfg.Identity.BootstrapToken)) < 32 {
		return fmt.Errorf("IAM_BOOTSTRAP_TOKEN must be at least 32 characters when set")
	}
	if cfg.Auth.SessionCookieName == "" {
		return fmt.Errorf("AUTH_SESSION_COOKIE_NAME must not be empty")
	}
	if cfg.Auth.SessionTTL <= 0 {
		return fmt.Errorf("AUTH_SESSION_TTL must be greater than zero")
	}
	if !validSameSite(cfg.Auth.SessionCookieSameSite) {
		return fmt.Errorf("AUTH_SESSION_COOKIE_SAME_SITE must be Lax, Strict or None")
	}
	if strings.EqualFold(cfg.Auth.SessionCookieSameSite, "none") && !cfg.Auth.SessionCookieSecure {
		return fmt.Errorf("AUTH_SESSION_COOKIE_SECURE must be true when AUTH_SESSION_COOKIE_SAME_SITE is None")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Environment), "production") {
		if !cfg.Auth.SessionCookieSecure {
			return fmt.Errorf("AUTH_SESSION_COOKIE_SECURE must be true when APP_ENV is production")
		}
		if !strings.EqualFold(publicBaseURL.Scheme, "https") {
			return fmt.Errorf("APP_PUBLIC_BASE_URL must use HTTPS when APP_ENV is production")
		}
		if !strings.EqualFold(oidcIssuer.Scheme, "https") {
			return fmt.Errorf("OIDC_ISSUER must use HTTPS when APP_ENV is production")
		}
	}
	if cfg.Logging.Directory == "" || cfg.FileStorageRoot == "" {
		return fmt.Errorf("LOG_DIRECTORY and FILE_STORAGE_ROOT must not be empty")
	}
	if strings.TrimSpace(cfg.Worker.ID) == "" || cfg.Worker.PollInterval <= 0 || cfg.Worker.StaleLockTimeout <= 0 {
		return fmt.Errorf("async worker configuration is invalid")
	}
	if strings.TrimSpace(cfg.Audit.ApplicationCode) == "" || strings.TrimSpace(cfg.Audit.EnvironmentCode) == "" {
		return fmt.Errorf("AUDIT_APPLICATION_CODE and AUDIT_ENVIRONMENT_CODE must not be empty")
	}
	if cfg.SubsystemOnboarding.Enabled {
		if strings.TrimSpace(cfg.SubsystemOnboarding.SocketPath) == "" ||
			strings.TrimSpace(cfg.SubsystemOnboarding.PlatformComposeProject) == "" ||
			strings.TrimSpace(cfg.SubsystemOnboarding.DockerBinary) == "" || cfg.SubsystemOnboarding.Timeout <= 0 {
			return fmt.Errorf("subsystem onboarding automation configuration is incomplete")
		}
		switch cfg.SubsystemOnboarding.Mode {
		case "local":
			if strings.TrimSpace(cfg.SubsystemOnboarding.ProjectsRoot) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.GatewayScriptPath) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.GatewayIncludePath) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.PlatformFrontendService) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.PlatformDockerNetwork) == "" {
				return fmt.Errorf("local subsystem onboarding automation configuration is incomplete")
			}
		case "production":
			if strings.TrimSpace(cfg.SubsystemOnboarding.ProductionDeployRoot) == "" || strings.TrimSpace(cfg.SubsystemOnboarding.ProductionAllowedTenant) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.ProductionApplicationCode) == "" || strings.TrimSpace(cfg.SubsystemOnboarding.ProductionApplicationName) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.ProductionEnvironment) == "" || strings.TrimSpace(cfg.SubsystemOnboarding.ProductionUpstreamURL) == "" ||
				strings.TrimSpace(cfg.SubsystemOnboarding.ProductionPathPrefix) == "" || strings.TrimSpace(cfg.SubsystemOnboarding.ProductionClientType) == "" {
				return fmt.Errorf("production subsystem onboarding automation configuration is incomplete")
			}
		default:
			return fmt.Errorf("SUBSYSTEM_ONBOARDING_MODE must be local or production")
		}
	}
	if len(cfg.CORSOrigins) == 0 {
		return fmt.Errorf("APP_CORS_ALLOWED_ORIGINS must contain at least one exact origin")
	}
	for _, origin := range cfg.CORSOrigins {
		if origin == "*" {
			return fmt.Errorf("APP_CORS_ALLOWED_ORIGINS must not contain wildcard origin when credentials are enabled")
		}
		parsed, err := url.ParseRequestURI(origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("APP_CORS_ALLOWED_ORIGINS contains invalid origin %q", origin)
		}
	}
	return nil
}

func validTrustedProxy(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "/") {
		_, _, err := net.ParseCIDR(value)
		return err == nil
	}
	return net.ParseIP(value) != nil
}

// resolveEnvFile 优先采用显式 ENV_FILE；未覆盖时从 backend 工作目录定位仓库根文件。
// 候选文件不存在仍是合法状态，因为生产部署可以完全依赖进程环境或密钥管理系统。
func resolveEnvFile() string {
	if path, exists := os.LookupEnv("ENV_FILE"); exists {
		return strings.TrimSpace(path)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return defaultEnvFile
	}
	return defaultEnvFilePath(workingDirectory)
}

func defaultEnvFilePath(workingDirectory string) string {
	candidates := []string{
		filepath.Join(workingDirectory, defaultEnvFile),
		filepath.Join(workingDirectory, "..", defaultEnvFile),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return candidates[0]
}

// resolveConfigPath 将相对路径锚定到选中的环境文件目录，而不是当前工作目录，
// 避免同一配置由 API、迁移命令或 Worker 从不同目录启动时解析成不同文件。
func resolveConfigPath(envFile, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(filepath.Dir(envFile), value))
}

func value(key, fallback string) string {
	if raw, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(raw)
	}
	return fallback
}

// valueOrDefault returns fallback when the variable is unset or intentionally blank.
// It is used for optional settings whose safe default is derived from another setting.
func valueOrDefault(key, fallback string) string {
	value := value(key, "")
	if value == "" {
		return fallback
	}
	return value
}

func integer(key string, fallback int) (int, error) {
	raw := value(key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func boolean(key string, fallback bool) (bool, error) {
	raw := value(key, strconv.FormatBool(fallback))
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := value(key, fallback.String())
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", key, err)
	}
	return parsed, nil
}

func commaSeparated(raw string) []string {
	items := strings.Split(raw, ",")
	values := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func validSameSite(value string) bool {
	switch strings.ToLower(value) {
	case "lax", "strict", "none":
		return true
	default:
		return false
	}
}
