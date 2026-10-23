package infrastructure

import (
	"context"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 批次 3：审计检索必须支持按操作地址、对象、链路 ID 过滤；
// client_ip 为空的历史行回退 source_ip，非法地址返回空结果而不是 500。
func newAuditQueryDryRun(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return db
}

func auditQuerySQL(t *testing.T, query application.PageRequest) string {
	t.Helper()
	repository := &Repository{database: newAuditQueryDryRun(t)}
	statement := repository.eventQuery(contextBackground(), "tenant-1", query).Find(&[]eventModel{}).Statement
	return statement.SQL.String()
}

func TestAuditQueryFiltersByClientIPWithLegacyFallback(t *testing.T) {
	sql := auditQuerySQL(t, application.PageRequest{Page: 1, PageSize: 20, ClientIP: "203.0.113.10"})
	if !strings.Contains(sql, "audit_event.client_ip = ?") || !strings.Contains(sql, "audit_event.source_ip = ?") {
		t.Fatalf("client_ip 过滤必须兼容历史 source_ip：%s", sql)
	}
}

func TestAuditQueryRejectsMalformedClientIP(t *testing.T) {
	sql := auditQuerySQL(t, application.PageRequest{Page: 1, PageSize: 20, ClientIP: "not-an-ip"})
	if !strings.Contains(sql, "1 = 0") {
		t.Fatalf("非法地址应返回空结果：%s", sql)
	}
}

func TestAuditQueryFiltersByResourceAndRequestID(t *testing.T) {
	sql := auditQuerySQL(t, application.PageRequest{Page: 1, PageSize: 20, ResourceID: "contract-1", RequestID: "req-1"})
	if !strings.Contains(sql, "audit_event.resource_id = ?") || !strings.Contains(sql, "audit_event.request_id = ?") {
		t.Fatalf("对象与链路过滤缺失：%s", sql)
	}
}

func contextBackground() context.Context { return context.Background() }
