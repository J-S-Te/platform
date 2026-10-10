package infrastructure

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestOfflineBootstrapProjectionUsesPersistedActiveBinding(t *testing.T) {
	// DryRun and disabled initialization/ping guarantee no database is contacted.
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "unused:unused@tcp(127.0.0.1:1)/unused", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	statement := offlineBootstrapProjectionQuery(db, now).Find(&principalProjection{}).Statement
	sql := statement.SQL.String()
	for _, fragment := range []string{"iam_bootstrap_state AS bootstrap", "bootstrap.first_super_admin_account_id", "account.user_id = bootstrap.first_super_admin_user_id", "tenant.code = ?", "tenant.status = ?", "account.status = ?", "account.valid_until > ?", "account.locked_until <= ?", "user.deleted_at IS NULL", "user.status = ?", "user.valid_until > ?", "LIMIT ?"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("missing maintenance binding restriction %q", fragment)
		}
	}
	if strings.Contains(sql, "iam_session") {
		t.Fatal("maintenance creates or depends on a browser session")
	}
	if len(statement.Vars) != 8 || statement.Vars[0] != "default" || statement.Vars[1] != "ACTIVE" || statement.Vars[2] != "ACTIVE" || statement.Vars[5] != "ACTIVE" || statement.Vars[7] != 1 {
		t.Fatalf("uncontrolled tenant restriction %#v", statement.Vars)
	}
	for _, index := range []int{3, 4, 6} {
		if statement.Vars[index] != now.UTC() {
			t.Fatalf("validity restriction %d does not use trusted current time", index)
		}
	}
}
