package migrations_test

import (
	"database/sql"
	"net"
	"os"
	"testing"

	"github.com/J-S-Te/Basic-Platform/migrations"
	driver "github.com/go-sql-driver/mysql"
)

func TestMySQLManagedRefreshRepairBoundaries(t *testing.T) {
	dsn := os.Getenv("MYSQL_REFRESH_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated MYSQL_REFRESH_TEST_DSN not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid dedicated test DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "platform_refresh_test" {
		t.Fatal("only the disposable loopback refresh test database is allowed")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open dedicated test database")
	}
	defer db.Close()
	// Bounded projection exercises the actual migration SQL and binary identifier
	// comparisons. Full schema application is additionally tested by the installer.
	for _, ddl := range []string{
		`CREATE TABLE platform_application (id VARCHAR(26) COLLATE ascii_bin PRIMARY KEY,tenant_id VARCHAR(26) COLLATE ascii_bin,code VARCHAR(64) COLLATE ascii_bin)`,
		`CREATE TABLE platform_application_environment (id VARCHAR(26) COLLATE ascii_bin PRIMARY KEY,tenant_id VARCHAR(26) COLLATE ascii_bin,application_id VARCHAR(26) COLLATE ascii_bin,environment VARCHAR(32) COLLATE ascii_bin,issuer_alias VARCHAR(128))`,
		`CREATE TABLE platform_oauth_client (id VARCHAR(26) COLLATE ascii_bin PRIMARY KEY,tenant_id VARCHAR(26) COLLATE ascii_bin,application_id VARCHAR(26) COLLATE ascii_bin,environment_id VARCHAR(26) COLLATE ascii_bin,client_id VARCHAR(128) COLLATE ascii_bin,client_type VARCHAR(32),token_auth_method VARCHAR(64),refresh_token_ttl_seconds INT UNSIGNED,require_pkce BOOL,status VARCHAR(32),version BIGINT UNSIGNED,updated_at DATETIME(3))`,
		`CREATE TABLE platform_oauth_grant_type (oauth_client_id VARCHAR(26) COLLATE ascii_bin,grant_type VARCHAR(64) COLLATE ascii_bin)`,
		`CREATE TABLE platform_oauth_client_scope (oauth_client_id VARCHAR(26) COLLATE ascii_bin,scope_code VARCHAR(128) COLLATE ascii_bin)`,
	} {
		if _, err = db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	cases := []string{"repair", "positive-ttl", "keycloak", "wrong-name", "service", "no-pkce", "disabled", "no-refresh", "no-code", "no-openid", "cross-tenant", "cross-app"}
	for _, kind := range cases {
		tenant, environmentTenant, environmentApp, code, alias := "tenant", "tenant", kind, "contract_management", "platform"
		clientID, clientType, pkce, status, ttl := "contract_management-prod-web", "confidential", true, "ACTIVE", 0
		switch kind {
		case "positive-ttl":
			ttl = 900
		case "keycloak":
			alias = "keycloak"
		case "wrong-name":
			clientID = "custom-client"
		case "service":
			clientType = "service"
		case "no-pkce":
			pkce = false
		case "disabled":
			status = "DISABLED"
		case "cross-tenant":
			environmentTenant = "other"
		case "cross-app":
			environmentApp = "other"
		}
		if _, err = db.Exec(`INSERT INTO platform_application VALUES (?,?,?)`, kind, tenant, code); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO platform_application_environment VALUES (?,?,?,?,?)`, kind, environmentTenant, environmentApp, "prod", alias); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO platform_oauth_client VALUES (?,?,?,?,?,?,?,?,?,?,1,UTC_TIMESTAMP(3))`, kind, tenant, kind, kind, clientID, clientType, "client_secret_basic", ttl, pkce, status); err != nil {
			t.Fatal(err)
		}
		for _, grant := range []string{"authorization_code", "refresh_token"} {
			if (kind == "no-refresh" && grant == "refresh_token") || (kind == "no-code" && grant == "authorization_code") {
				continue
			}
			if _, err = db.Exec(`INSERT INTO platform_oauth_grant_type VALUES (?,?)`, kind, grant); err != nil {
				t.Fatal(err)
			}
		}
		if kind != "no-openid" {
			if _, err = db.Exec(`INSERT INTO platform_oauth_client_scope VALUES (?,?)`, kind, "openid"); err != nil {
				t.Fatal(err)
			}
		}
	}
	query, err := migrations.Files.ReadFile("000120_repair_managed_browser_refresh_ttl.sql")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = db.Exec(string(query)); err != nil {
			t.Fatal(err)
		}
		for _, kind := range cases {
			var ttl, version int
			if err = db.QueryRow(`SELECT refresh_token_ttl_seconds,version FROM platform_oauth_client WHERE id=?`, kind).Scan(&ttl, &version); err != nil {
				t.Fatal(err)
			}
			wantTTL, wantVersion := 0, 1
			if kind == "repair" {
				wantTTL, wantVersion = 2592000, 2
			}
			if kind == "positive-ttl" {
				wantTTL = 900
			}
			if ttl != wantTTL || version != wantVersion {
				t.Fatalf("%s attempt %d: ttl=%d version=%d", kind, attempt, ttl, version)
			}
		}
	}
}
