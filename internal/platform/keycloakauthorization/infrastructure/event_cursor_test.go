package infrastructure

import (
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 批次 3：采集游标按 realm 持久化，字段必须与迁移一致；
// 本地无隔离 MySQL，用 DryRun 解析表结构断言列名。
func TestEventCursorModelMatchesMigration(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	statement := db.Model(&eventCursorModel{}).Find(&[]eventCursorModel{}).Statement
	if statement.Table != "keycloak_event_cursor" {
		t.Fatalf("游标表 = %q，want keycloak_event_cursor", statement.Table)
	}
	if statement.Schema == nil {
		t.Fatal("未解析出游标表结构")
	}
	columns := map[string]bool{}
	for _, field := range statement.Schema.Fields {
		columns[field.DBName] = true
	}
	for _, column := range []string{"realm", "last_event_time", "last_event_id", "consecutive_failures", "last_error", "updated_at"} {
		if !columns[column] {
			t.Errorf("游标模型缺少列 %q", column)
		}
	}
}

// 没有游标记录时返回零游标（采集器据此从回看窗口开始），而不是报错。
func TestZeroEventCursorStartsFromWindow(t *testing.T) {
	cursor := EventCursor{Realm: "basic-platform"}
	if !cursor.LastEventTime.IsZero() || cursor.LastEventID != "" || cursor.ConsecutiveFailures != 0 {
		t.Fatalf("零游标 = %#v", cursor)
	}
}
