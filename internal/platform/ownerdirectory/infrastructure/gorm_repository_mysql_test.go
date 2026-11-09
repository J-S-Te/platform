package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ownerKeywordDatabase 创建同连接临时表，复现真实的 ASCII 标识和中文姓名边界。
// 参数：t为测试上下文；DSN必须显式指向独立的owner_directory_test库。
// 返回值：仅使用临时表的数据库连接，连接关闭后夹具自动销毁。
func ownerKeywordDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_OWNER_DIRECTORY_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_OWNER_DIRECTORY_TEST_DSN is not configured")
	}
	config, err := drivermysql.ParseDSN(dsn)
	if err != nil || config.DBName != "owner_directory_test" {
		t.Fatal("owner directory tests require the dedicated owner_directory_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	createOwnerKeywordTables(t, db)
	return db
}

// createOwnerKeywordTables 建立目录查询所需的最小真实字符集及授权夹具。
// 参数：t为测试上下文，db为独立测试库的单连接。
// 返回值：无；临时表或夹具写入失败时终止测试。
func createOwnerKeywordTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	definitions := []string{
		"iam_bootstrap_state (tenant_id CHAR(26), first_super_admin_user_id CHAR(26), first_super_admin_account_id CHAR(26))",
		"iam_tenant (id CHAR(26), code VARCHAR(32))",
		"iam_user (id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin, tenant_id CHAR(26), display_name VARCHAR(128), status VARCHAR(16), employment_status VARCHAR(32), deleted_at DATETIME NULL)",
		"iam_org_unit (id CHAR(26), tenant_id CHAR(26), name VARCHAR(128), status VARCHAR(16))",
		"iam_position (id CHAR(26), tenant_id CHAR(26), org_unit_id CHAR(26), status VARCHAR(16))",
		"iam_membership (tenant_id CHAR(26), user_id CHAR(26), org_unit_id CHAR(26), position_id CHAR(26), status VARCHAR(16), valid_from DATETIME NULL, valid_until DATETIME NULL, inherit_authorization BOOL, is_primary BOOL)",
		"authz_role (id CHAR(26), tenant_id CHAR(26), application_id CHAR(26), status VARCHAR(16), role_type VARCHAR(32))",
		"authz_role_binding (tenant_id CHAR(26), application_id CHAR(26), role_id CHAR(26), status VARCHAR(16), valid_from DATETIME NULL, valid_until DATETIME NULL, scope_type VARCHAR(32), scope_id VARCHAR(128), subject_type VARCHAR(32), subject_id CHAR(26))",
	}
	for _, definition := range definitions {
		if err := db.Exec("CREATE TEMPORARY TABLE " + definition +
			" DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci").Error; err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		"INSERT INTO iam_tenant VALUES ('tenant','owner-test')",
		"INSERT INTO iam_org_unit VALUES ('org','tenant','测试部门','ACTIVE')",
		"INSERT INTO iam_user VALUES ('01K00000000000000000000001','tenant','负责人甲','ACTIVE','EMPLOYEE',NULL), ('01K00000000000000000000002','tenant','未授权人员','ACTIVE','EMPLOYEE',NULL), ('01K00000000000000000000003','tenant','A_%人员','ACTIVE','EMPLOYEE',NULL)",
		"INSERT INTO iam_membership VALUES ('tenant','01K00000000000000000000001','org','position','ACTIVE',NULL,NULL,0,1), ('tenant','01K00000000000000000000002','org','position','ACTIVE',NULL,NULL,0,1), ('tenant','01K00000000000000000000003','org','position','ACTIVE',NULL,NULL,0,1)",
		"INSERT INTO authz_role VALUES ('role','tenant','app','ACTIVE','CUSTOM')",
		"INSERT INTO authz_role_binding VALUES ('tenant','app','role','ACTIVE',NULL,NULL,'ENVIRONMENT','prod','USER','01K00000000000000000000001'), ('tenant','app','role','ACTIVE',NULL,NULL,'ENVIRONMENT','prod','USER','01K00000000000000000000003')",
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// TestOwnerDirectoryKeywordMySQL 验证中文搜索、标识查找和授权边界在真实MySQL下兼容。
// 参数：t为测试上下文，数据库由显式独立测试DSN提供。
// 返回值：无；字符集错误、模糊通配扩大结果或跨环境返回用户时失败。
func TestOwnerDirectoryKeywordMySQL(t *testing.T) {
	db := ownerKeywordDatabase(t)
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, keyword, environment, wantID string
	}{
		{"中文姓名", "负责人甲", "prod", "01K00000000000000000000001"},
		{"中文部分匹配", "负责人", "prod", "01K00000000000000000000001"},
		{"用户标识", "01K00000000000000000000001", "prod", "01K00000000000000000000001"},
		{"中文无匹配", "不存在的人", "prod", ""},
		{"未授权姓名", "未授权人员", "prod", ""},
		{"其他环境", "负责人甲", "other", ""},
		{"未授权标识", "01K00000000000000000000002", "prod", ""},
		{"标识其他环境", "01K00000000000000000000001", "other", ""},
		{"字面通配符", "_%", "prod", "01K00000000000000000000003"},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := repository.List(context.Background(), "tenant", "app", test.environment,
				application.Query{Keyword: test.keyword, Page: 1, PageSize: 20})
			if err != nil {
				t.Fatalf("directory keyword lookup: %v", err)
			}
			if test.wantID == "" {
				if page.Total != 0 || len(page.Items) != 0 {
					t.Fatalf("unexpected candidates: %+v", page)
				}
				return
			}
			if page.Total != 1 || len(page.Items) != 1 ||
				strings.TrimSpace(page.Items[0].ID) != test.wantID {
				t.Fatalf("unexpected candidates: %+v", page)
			}
		})
	}
}
