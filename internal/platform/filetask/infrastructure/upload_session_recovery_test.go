package infrastructure

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// V2 上传会话的恢复写入必须与 HTTP 写入同一张表、同一组列名，并且只能收敛仍处于
// UPLOADING 的会话：文件对账是周期性任务，重复执行不能覆盖已终结的会话。

func newFileTaskDryRun(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return database
}

func TestPendingUploadSessionQueryIsTenantAndFileScoped(t *testing.T) {
	database := newFileTaskDryRun(t)
	statement := database.Model(&uploadV2SessionModel{}).
		Where("tenant_id = ? AND file_id = ? AND status = ?", "tenant-1", "file-1", "UPLOADING").
		Order("created_at ASC").Take(&uploadV2SessionModel{}).Statement
	sql := statement.SQL.String()
	for _, fragment := range []string{"`file_upload_v2_session`", "tenant_id = ?", "file_id = ?", "status = ?"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("待恢复会话查询缺少 %q：%s", fragment, sql)
		}
	}
	if statement.Table != "file_upload_v2_session" {
		t.Fatalf("会话表名 = %q, want file_upload_v2_session", statement.Table)
	}
}

func TestMarkUploadSessionOnlyConvergesUploadingRows(t *testing.T) {
	database := newFileTaskDryRun(t)
	now := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)

	ready := database.Model(&uploadV2SessionModel{}).
		Where("id = ? AND tenant_id = ? AND status = ?", "session-1", "tenant-1", "UPLOADING").
		Updates(map[string]any{"status": "READY", "completed_at": now, "failure_code": nil, "updated_at": now})
	if ready.Error != nil {
		t.Fatalf("mark session ready: %v", ready.Error)
	}
	sql := ready.Statement.SQL.String()
	if !strings.Contains(sql, "UPDATE `file_upload_v2_session`") || !strings.Contains(sql, "status = ?") {
		t.Fatalf("会话收敛必须限定当前状态，避免覆盖已终结会话：%s", sql)
	}
	if !strings.Contains(sql, "completed_at") || !strings.Contains(sql, "failure_code") {
		t.Fatalf("READY 收敛必须写入 completed_at 并清空 failure_code：%s", sql)
	}

	failed := database.Model(&uploadV2SessionModel{}).
		Where("id = ? AND tenant_id = ? AND status = ?", "session-1", "tenant-1", "UPLOADING").
		Updates(map[string]any{"status": "FAILED", "failure_code": "BINDING_FAILED", "updated_at": now})
	if failed.Error != nil {
		t.Fatalf("mark session failed: %v", failed.Error)
	}
	if !strings.Contains(failed.Statement.SQL.String(), "failure_code") {
		t.Fatalf("失败收敛必须记录 failure_code：%s", failed.Statement.SQL.String())
	}
}

// 会话表列名必须与迁移和 HTTP 模型一致，否则恢复写入会命中不存在的列。
func TestUploadSessionRecoveryModelColumnsMatchSchema(t *testing.T) {
	statement := newFileTaskDryRun(t).Model(&uploadV2SessionModel{}).Find(&[]uploadV2SessionModel{}).Statement
	if statement.Schema == nil {
		t.Fatal("未解析出上传会话表结构")
	}
	columns := map[string]bool{}
	for _, field := range statement.Schema.Fields {
		columns[field.DBName] = true
	}
	for _, column := range []string{"id", "tenant_id", "application_id", "actor_user_id", "file_id", "resource_type", "resource_id", "binding_type", "display_name", "original_name", "status", "failure_code", "completed_at", "updated_at"} {
		if !columns[column] {
			t.Errorf("恢复模型缺少列 %q", column)
		}
	}
}
