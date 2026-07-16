// Package database owns infrastructure-level database connection construction.
package database

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	mysql "github.com/go-sql-driver/mysql"
)

// OpenMySQL creates a pooled MySQL database handle. sql.Open does not establish a network
// connection; readiness checks perform PingContext so the API can still expose liveness when
// MySQL is temporarily unavailable.
func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
	params, err := parseParameters(cfg.Params)
	if err != nil {
		return nil, err
	}

	driverConfig := mysql.NewConfig()
	driverConfig.User = cfg.Username
	driverConfig.Passwd = cfg.Password
	driverConfig.Net = "tcp"
	driverConfig.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	driverConfig.DBName = cfg.Database
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Params = params

	db, err := sql.Open("mysql", driverConfig.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql database handle: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	return db, nil
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
