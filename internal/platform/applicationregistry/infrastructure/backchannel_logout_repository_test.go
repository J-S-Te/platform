package infrastructure

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 显式 opt-out（空 URI）不写登记行也不报错；不受支持的端点（非回环明文 HTTP、
// 带 query/fragment）一律拒绝。两者都在触库之前判定，因此用 nil 事务即可断言。
func TestUpsertBackchannelLogoutURIWithinGuardsInput(t *testing.T) {
	now := time.Now().UTC()
	if err := UpsertBackchannelLogoutURIWithin(nil, "01KOAUTH000000000000000001", "https://rp.example.com/logout", now); err == nil {
		t.Fatal("nil transaction was accepted")
	}
	if err := UpsertBackchannelLogoutURIWithin(nil, "01KOAUTH000000000000000001", "  ", now); err != nil {
		t.Fatalf("empty URI (explicit opt-out) must not write: %v", err)
	}
	for _, uri := range []string{"http://rp.example.com/logout", "https://rp.example.com/logout?next=1", "https://user@rp.example.com/logout"} {
		if err := UpsertBackchannelLogoutURIWithin(nil, "01KOAUTH000000000000000001", uri, now); !errors.Is(err, application.ErrInvalidBackchannelLogoutURI) {
			t.Fatalf("URI %q err = %v", uri, err)
		}
	}
}

// 登记写语句必须保持幂等：接入、管理 API 与存量回填重放都走同一条 upsert。
func TestBackchannelLogoutURIUpsertSQLIsIdempotent(t *testing.T) {
	normalized := strings.Join(strings.Fields(backchannelLogoutURIUpsertSQL), " ")
	for _, fragment := range []string{"INSERT INTO platform_oauth_backchannel_logout_uri", "ON DUPLICATE KEY UPDATE logout_uri=VALUES(logout_uri)"} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("upsert lost %q:\n%s", fragment, normalized)
		}
	}
}

// 真实数据库验证登记幂等性（受控重放补登记）：同一客户端重复登记只保留一行且取最新值。
func TestBackchannelLogoutURIUpsertIsIdempotentOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS platform_oauth_backchannel_logout_uri",
		"DROP TABLE IF EXISTS platform_oauth_client",
		`CREATE TABLE platform_oauth_client (
			id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, status VARCHAR(16) NOT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE platform_oauth_backchannel_logout_uri (
			oauth_client_id CHAR(26) NOT NULL, logout_uri VARCHAR(2048) NOT NULL,
			created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			PRIMARY KEY (oauth_client_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if err := database.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	const clientID = "01KOAUTH000000000000000001"
	if err := database.Exec("INSERT INTO platform_oauth_client (id, tenant_id, status) VALUES (?,?,?)", clientID, "01KTENANT00000000000000001", "ACTIVE").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	registered := func() string {
		t.Helper()
		var rows []string
		if err := database.Table("platform_oauth_backchannel_logout_uri").Where("oauth_client_id = ?", clientID).Pluck("logout_uri", &rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("registered rows = %v, want exactly 1", rows)
		}
		return rows[0]
	}
	if err := database.Transaction(func(tx *gorm.DB) error {
		return UpsertBackchannelLogoutURIWithin(tx, clientID, "https://rp.example.com/auth/backchannel-logout", now)
	}); err != nil {
		t.Fatal(err)
	}
	if got := registered(); got != "https://rp.example.com/auth/backchannel-logout" {
		t.Fatalf("registered URI = %q", got)
	}
	// 幂等重放（域名迁移/受控更新）原地更新，不产生第二行也不撞唯一键。
	if err := database.Transaction(func(tx *gorm.DB) error {
		return UpsertBackchannelLogoutURIWithin(tx, clientID, "https://platform.example.com/rp/auth/backchannel-logout", now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if got := registered(); got != "https://platform.example.com/rp/auth/backchannel-logout" {
		t.Fatalf("replayed URI = %q", got)
	}
}
