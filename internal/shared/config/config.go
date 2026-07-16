// Package config loads process configuration from the root .env file and the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultEnvFile = ".env"

// Config contains process-level configuration. Runtime business configuration belongs in the
// configuration module and must not be added to this type.
type Config struct {
	Environment     string
	AppName         string
	Timezone        string
	HTTP            HTTPConfig
	MySQL           MySQLConfig
	Auth            AuthConfig
	Logging         LoggingConfig
	FileStorageRoot string
	CORSOrigins     []string
}

// HTTPConfig controls the API listener and public address.
type HTTPConfig struct {
	Addr          string
	PublicBaseURL string
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
	JWTIssuer             string
	JWTAudience           string
	JWTPrivateKeyPath     string
	JWTPublicKeyPath      string
	SessionCookieName     string
	SessionCookieSecure   bool
	SessionCookieSameSite string
	SessionTTL            time.Duration
}

// LoggingConfig controls structured application logs.
type LoggingConfig struct {
	Level     string
	Format    string
	Directory string
}

// Load reads the project-root .env file when present. Environment variables always take
// precedence over values in .env, which keeps container and deployment configuration explicit.
func Load() (Config, error) {
	envFile := value("ENV_FILE", defaultEnvFile)
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

	sessionTTL, err := duration("AUTH_SESSION_TTL", 8*time.Hour)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Environment: value("APP_ENV", "development"),
		AppName:     value("APP_NAME", "basic-platform"),
		Timezone:    value("APP_TIMEZONE", "Asia/Shanghai"),
		HTTP: HTTPConfig{
			Addr:          value("APP_HTTP_ADDR", ":8080"),
			PublicBaseURL: value("APP_PUBLIC_BASE_URL", "http://localhost:8080"),
		},
		MySQL: MySQLConfig{
			Host:     value("MYSQL_HOST", "127.0.0.1"),
			Port:     mysqlPort,
			Database: value("MYSQL_DATABASE", "basic_platform"),
			Username: value("MYSQL_USERNAME", "basic_platform"),
			Password: value("MYSQL_PASSWORD", ""),
			Params:   value("MYSQL_PARAMS", "charset=utf8mb4&parseTime=true&loc=UTC"),
		},
		Auth: AuthConfig{
			JWTIssuer:             value("AUTH_JWT_ISSUER", "basic-platform"),
			JWTAudience:           value("AUTH_JWT_AUDIENCE", "basic-platform-console"),
			JWTPrivateKeyPath:     value("AUTH_JWT_PRIVATE_KEY_PATH", ""),
			JWTPublicKeyPath:      value("AUTH_JWT_PUBLIC_KEY_PATH", ""),
			SessionCookieName:     value("AUTH_SESSION_COOKIE_NAME", "bp_session"),
			SessionCookieSecure:   cookieSecure,
			SessionCookieSameSite: value("AUTH_SESSION_COOKIE_SAME_SITE", "Lax"),
			SessionTTL:            sessionTTL,
		},
		Logging: LoggingConfig{
			Level:     value("LOG_LEVEL", "info"),
			Format:    value("LOG_FORMAT", "json"),
			Directory: value("LOG_DIRECTORY", filepath.Join("data", "logs")),
		},
		FileStorageRoot: value("FILE_STORAGE_ROOT", filepath.Join("data", "uploads")),
		CORSOrigins:     commaSeparated(value("APP_CORS_ALLOWED_ORIGINS", "http://localhost:5173")),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate rejects configuration that would make the process unsafe or impossible to start.
func (cfg Config) Validate() error {
	if cfg.AppName == "" {
		return fmt.Errorf("APP_NAME must not be empty")
	}
	if cfg.HTTP.Addr == "" {
		return fmt.Errorf("APP_HTTP_ADDR must not be empty")
	}
	if _, err := url.ParseRequestURI(cfg.HTTP.PublicBaseURL); err != nil {
		return fmt.Errorf("APP_PUBLIC_BASE_URL is invalid: %w", err)
	}
	if cfg.MySQL.Host == "" || cfg.MySQL.Database == "" || cfg.MySQL.Username == "" {
		return fmt.Errorf("MYSQL_HOST, MYSQL_DATABASE and MYSQL_USERNAME must not be empty")
	}
	if cfg.MySQL.Port < 1 || cfg.MySQL.Port > 65535 {
		return fmt.Errorf("MYSQL_PORT must be between 1 and 65535")
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
	if cfg.Logging.Directory == "" || cfg.FileStorageRoot == "" {
		return fmt.Errorf("LOG_DIRECTORY and FILE_STORAGE_ROOT must not be empty")
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

func value(key, fallback string) string {
	if raw, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(raw)
	}
	return fallback
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
