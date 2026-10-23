package infrastructure

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 角色列表 JOIN 了 platform_application，而两张表都有 code/name/status/created_at。
// 未限定的列名会让 MySQL 直接报 ambiguous：关键字筛选、状态筛选和排序全部失败。
func TestRoleListQueryQualifiesJoinedColumns(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}

	statement := roleListQuery(database, "tenant-1", application.PageRequest{Page: 1, PageSize: 10, Keyword: "销售", Status: "ACTIVE"}).
		Order("authz_role.created_at DESC").Limit(10).Find(&[]roleModel{}).Statement
	sql := statement.SQL.String()

	for _, fragment := range []string{"authz_role.code LIKE", "authz_role.name LIKE", "authz_role.status", "authz_role.tenant_id", "application.code"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("角色列表查询缺少限定列 %q: %s", fragment, sql)
		}
	}
	// 未限定的 code/name/status 会让 MySQL 报 ambiguous。
	if strings.Contains(sql, "WHERE (code LIKE") || strings.Contains(sql, " OR name LIKE") || strings.Contains(sql, " AND status =") {
		t.Fatalf("角色列表查询含未限定歧义列: %s", sql)
	}
}

func TestRoleListQueryWithoutFiltersStillScopesTenantAndApplication(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	sql := roleListQuery(database, "tenant-1", application.PageRequest{Page: 1, PageSize: 10}).Find(&[]roleModel{}).Statement.SQL.String()
	if !strings.Contains(sql, "authz_role.tenant_id = ?") || !strings.Contains(sql, "application.code = ?") {
		t.Fatalf("角色列表查询缺少租户或应用范围: %s", sql)
	}
	if strings.Contains(sql, "LIKE") {
		t.Fatalf("未提供关键字时不应生成 LIKE 条件: %s", sql)
	}
}
