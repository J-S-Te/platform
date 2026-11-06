package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCentralSessionFlagRejectsTypo 验证平台总开关不因拼写错误而静默退回分散计时。
// 参数：t为配置测试上下文。
// 返回值：无；非法布尔值被接受或错误未指明配置项时失败。
func TestCentralSessionFlagRejectsTypo(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("AUTH_CENTRAL_SESSION_ENABLED", "treu")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AUTH_CENTRAL_SESSION_ENABLED") {
		t.Fatal("central governance flag must reject invalid input")
	}
}

// TestCentralSessionsRequireEightHourPlatformCookie 验证平台Cookie不会提前截断中央周期。
// 参数：t为配置测试上下文。
// 返回值：无；启用中央管理却接受短平台绝对期限时失败。
func TestCentralSessionsRequireEightHourPlatformCookie(t *testing.T) {
	cfg := productionInsecureCookieConfig("production")
	cfg.Auth.CentralSessionEnabled = true
	cfg.Auth.SessionTTL = 15 * time.Minute
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "AUTH_SESSION_TTL") {
		t.Fatal("central governance requires platform TTL of eight hours")
	}
}
