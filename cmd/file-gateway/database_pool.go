package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type gatewayPoolSettings struct {
	maxOpen     int
	maxIdle     int
	maxLifetime time.Duration
	maxIdleTime time.Duration
}

// loadGatewayPool 读取文件网关独立且有界的连接池预算。
// 参数：无；配置来自FILE_GATEWAY_DB_环境变量。
// 返回值：经校验的预算；非法连接数量和非正生命周期返回错误。
func loadGatewayPool() (gatewayPoolSettings, error) {
	cfg := gatewayPoolSettings{maxOpen: 20, maxIdle: 5, maxLifetime: 15 * time.Minute, maxIdleTime: 2 * time.Minute}
	for key, target := range map[string]*int{"MAX_OPEN_CONNS": &cfg.maxOpen, "MAX_IDLE_CONNS": &cfg.maxIdle} {
		if raw := strings.TrimSpace(os.Getenv("FILE_GATEWAY_DB_" + key)); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return gatewayPoolSettings{}, fmt.Errorf("FILE_GATEWAY_DB_%s must be an integer", key)
			}
			*target = value
		}
	}
	for key, target := range map[string]*time.Duration{"CONN_MAX_LIFETIME": &cfg.maxLifetime, "CONN_MAX_IDLE_TIME": &cfg.maxIdleTime} {
		if raw := strings.TrimSpace(os.Getenv("FILE_GATEWAY_DB_" + key)); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return gatewayPoolSettings{}, fmt.Errorf("FILE_GATEWAY_DB_%s must be a positive duration", key)
			}
			*target = value
		}
	}
	if cfg.maxOpen <= 0 || cfg.maxIdle < 0 || cfg.maxIdle > cfg.maxOpen {
		return gatewayPoolSettings{}, fmt.Errorf("FILE_GATEWAY_DB_MAX_OPEN_CONNS must be positive; MAX_IDLE_CONNS must be between zero and max open")
	}
	if cfg.maxLifetime <= 0 || cfg.maxIdleTime <= 0 {
		return gatewayPoolSettings{}, fmt.Errorf("FILE_GATEWAY_DB_CONN_MAX_LIFETIME and CONN_MAX_IDLE_TIME must be positive")
	}
	return cfg, nil
}

// apply 将已校验预算应用到底层SQL池，不改变驱动探测行为。
// 参数：pool 为当前网关GORM句柄持有的SQL连接池。
// 返回值：无。
func (cfg gatewayPoolSettings) apply(pool *sql.DB) {
	pool.SetMaxOpenConns(cfg.maxOpen)
	pool.SetMaxIdleConns(cfg.maxIdle)
	pool.SetConnMaxLifetime(cfg.maxLifetime)
	pool.SetConnMaxIdleTime(cfg.maxIdleTime)
}

// openGatewayDatabase 保留网关GORM选项及自动启动Ping并设置独立池预算。
// 参数：dsn 为网关既有数据库连接配置。
// 返回值：已配置连接池的句柄；配置、连接或底层句柄错误直接返回。
func openGatewayDatabase(dsn string) (*gorm.DB, error) {
	cfg, err := loadGatewayPool()
	if err != nil {
		return nil, err
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true, SkipDefaultTransaction: true})
	if err != nil {
		return nil, err
	}
	pool, err := database.DB()
	if err != nil {
		return nil, fmt.Errorf("get file gateway database pool: %w", err)
	}
	cfg.apply(pool)
	return database, nil
}
