// Package database 统一构造基础设施层数据库连接，避免各业务模块自行拼接 DSN 或连接池参数。
package database

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	mysqldriver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// OpenMySQL 创建共享 GORM 句柄。启动阶段有意关闭自动 Ping：MySQL 暂时不可用时 API 仍能
// 提供 /healthz 表示进程存活，而 /readyz 再用有界查询表达依赖是否就绪。
func OpenMySQL(cfg config.MySQLConfig) (*gorm.DB, error) {
	params, err := parseParameters(cfg.Params)
	if err != nil {
		return nil, err
	}

	driverConfig := gormmysql.Config{
		DSN:                       buildDSN(cfg, params),
		SkipInitializeWithVersion: true,
	}

	database, err := gorm.Open(gormmysql.New(driverConfig), &gorm.Config{
		// 默认事务会给每次单语句写入增加额外开销；真正需要原子性的业务仓储必须显式开启事务。
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		TranslateError:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("open mysql database handle: %w", err)
	}

	sqlDatabase, err := database.DB()
	if err != nil {
		return nil, fmt.Errorf("access mysql database pool: %w", err)
	}
	// Keep enough headroom for the OIDC authorization-code exchange while leaving
	// room below MySQL's server-wide connection limit for the worker and other
	// platform processes. The previous 25-connection ceiling allowed a burst of
	// portal, audit, and subsystem requests to make token exchange wait for a
	// connection until Keycloak's five-second callback timeout elapsed.
	sqlDatabase.SetMaxOpenConns(50)
	sqlDatabase.SetMaxIdleConns(20)
	sqlDatabase.SetConnMaxLifetime(15 * time.Minute)
	sqlDatabase.SetConnMaxIdleTime(2 * time.Minute)

	return database, nil
}

// Close releases the underlying connection pool owned by a GORM database handle.
func Close(database *gorm.DB) error {
	if database == nil {
		return nil
	}

	sqlDatabase, err := database.DB()
	if err != nil {
		return fmt.Errorf("access mysql database pool for close: %w", err)
	}
	if err := sqlDatabase.Close(); err != nil {
		return fmt.Errorf("close mysql database pool: %w", err)
	}
	return nil
}

func buildDSN(cfg config.MySQLConfig, params map[string]string) string {
	driverConfig := mysqldriver.NewConfig()
	driverConfig.User = cfg.Username
	driverConfig.Passwd = cfg.Password
	driverConfig.Net = "tcp"
	driverConfig.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	driverConfig.DBName = cfg.Database
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Params = params
	return driverConfig.FormatDSN()
}

func parseParameters(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}

	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("parse MYSQL_PARAMS: %w", err)
	}

	params := make(map[string]string, len(values))
	for key, value := range values {
		if len(value) == 0 {
			continue
		}
		// 时间解析和时区属于平台级一致性约束，不能被 MYSQL_PARAMS 覆盖成驱动默认值或本地时区。
		if strings.EqualFold(key, "parseTime") || strings.EqualFold(key, "loc") {
			continue
		}
		params[key] = value[len(value)-1]
	}
	return params, nil
}
