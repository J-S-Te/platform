// Package database owns infrastructure-level database connection construction.
package database

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	mysqldriver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// OpenMySQL creates the shared GORM database handle. Automatic pinging is disabled so the API
// process can expose /healthz while MySQL is temporarily unavailable; /readyz verifies the
// dependency with a bounded query.
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
	sqlDatabase.SetMaxOpenConns(25)
	sqlDatabase.SetMaxIdleConns(10)
	sqlDatabase.SetConnMaxLifetime(30 * time.Minute)
	sqlDatabase.SetConnMaxIdleTime(5 * time.Minute)

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
		if strings.EqualFold(key, "parseTime") || strings.EqualFold(key, "loc") {
			continue
		}
		params[key] = value[len(value)-1]
	}
	return params, nil
}
