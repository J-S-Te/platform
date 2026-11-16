package main

import (
	"database/sql"
	"testing"
	"time"
)

// TestFileGatewayPoolSettings 验证独立文件网关预算不依赖平台API池。
// 参数：t 为当前测试上下文。
// 返回值：无；默认或覆盖配置不符合约定时失败。
func TestFileGatewayPoolSettings(t *testing.T) {
	clearGatewayPoolEnv(t)
	cfg, err := loadGatewayPool()
	if err != nil || cfg.maxOpen != 20 || cfg.maxIdle != 5 || cfg.maxLifetime != 15*time.Minute || cfg.maxIdleTime != 2*time.Minute {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	t.Setenv("FILE_GATEWAY_DB_MAX_OPEN_CONNS", "8")
	t.Setenv("FILE_GATEWAY_DB_MAX_IDLE_CONNS", "0")
	t.Setenv("FILE_GATEWAY_DB_CONN_MAX_LIFETIME", "8m")
	t.Setenv("FILE_GATEWAY_DB_CONN_MAX_IDLE_TIME", "30s")
	cfg, err = loadGatewayPool()
	if err != nil || cfg.maxOpen != 8 || cfg.maxIdle != 0 || cfg.maxLifetime != 8*time.Minute || cfg.maxIdleTime != 30*time.Second {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	raw, err := sql.Open("mysql", "test:test@tcp(127.0.0.1:1)/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cfg.apply(raw)
	if raw.Stats().MaxOpenConnections != 8 {
		t.Fatal("actual SQL pool was not bounded")
	}
}

// TestFileGatewayPoolRejectsInvalid 验证无限数量、非法时长和无效空闲预算失败关闭。
// 参数：t 为当前测试上下文。
// 返回值：无；非法配置被接受时失败。
func TestFileGatewayPoolRejectsInvalid(t *testing.T) {
	clearGatewayPoolEnv(t)
	for _, tc := range []struct{ key, value string }{
		{"MAX_OPEN_CONNS", "0"}, {"MAX_OPEN_CONNS", "bad"}, {"MAX_IDLE_CONNS", "-1"},
		{"MAX_IDLE_CONNS", "21"}, {"CONN_MAX_LIFETIME", "0"}, {"CONN_MAX_IDLE_TIME", "-1s"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv("FILE_GATEWAY_DB_"+tc.key, tc.value)
			if _, err := loadGatewayPool(); err == nil {
				t.Fatal("accepted invalid pool settings")
			}
		})
	}
}

// clearGatewayPoolEnv 隔离运行环境已有的网关连接池配置。
// 参数：t 为当前测试上下文。
// 返回值：无；测试结束后恢复原环境。
func clearGatewayPoolEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"MAX_OPEN_CONNS", "MAX_IDLE_CONNS", "CONN_MAX_LIFETIME", "CONN_MAX_IDLE_TIME"} {
		t.Setenv("FILE_GATEWAY_DB_"+key, "")
	}
}
