package infrastructure

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessRuntimeRegistrationOnMySQL 用途：核验真实表上的缺失、完整、旧协议和停用客户端登记。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无；数据仅存在于本次创建并自动删除的 schema。
func TestAccessRuntimeRegistrationOnMySQL(t *testing.T) {
	db := accessFullMigrationDatabase(t)
	now := time.Now().UTC()
	tenant, app, env := "01K10A00000000000000000001", "01K10B00000000000000000001", "01K10C00000000000000000001"
	seeds := []struct {
		table string
		row   map[string]any
	}{
		{"iam_tenant", map[string]any{"id": tenant, "code": "registration-test", "name": "隔离测试", "timezone": "Asia/Shanghai", "locale": "zh-CN", "status": "ACTIVE", "created_at": now, "updated_at": now}},
		{"platform_application", map[string]any{"id": app, "tenant_id": tenant, "code": "contract_management", "name": "隔离合同", "application_type": "INTERNAL", "status": "ACTIVE", "created_at": now, "updated_at": now}},
		{"platform_application_environment", map[string]any{"id": env, "tenant_id": tenant, "application_id": app, "environment": "prod", "status": "ACTIVE", "created_at": now, "updated_at": now}},
	}
	for _, seed := range seeds {
		if err := db.Table(seed.table).Create(seed.row).Error; err != nil {
			t.Fatal(err)
		}
	}
	images := []application.SubsystemImageReadiness{{ServiceID: "contract-api", Present: true}, {ServiceID: "contract-worker", Present: true}}
	tracker := &SubsystemAccessConfigurationTracker{database: db, registrationImages: imageReaderStub{images: images, reported: true}}
	check := func(want bool) {
		t.Helper()
		got, err := tracker.runtimeRegistrationMatches(context.Background(), tenant, "contract_management", "prod")
		if err != nil || got != want {
			t.Fatalf("registration=%v want=%v err=%v", got, want, err)
		}
	}
	check(false)
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), "contract_management", "prod")
	if err != nil {
		t.Fatal(err)
	}
	for i, service := range []string{"contract-api", "contract-worker"} {
		id := []string{"01K10D00000000000000000001", "01K10D00000000000000000002"}[i]
		client := "registration-" + service
		row := map[string]any{"id": id, "tenant_id": tenant, "application_id": app, "environment_id": env, "client_id": client, "client_name": service, "client_type": "CONFIDENTIAL", "token_auth_method": "client_secret_basic", "access_token_ttl_seconds": 300, "refresh_token_ttl_seconds": 0, "require_pkce": false, "status": "ACTIVE", "created_at": now, "updated_at": now}
		if err := db.Table("platform_oauth_client").Create(row).Error; err != nil {
			t.Fatal(err)
		}
		member := map[string]any{"service_id": service, "application": "contract_management", "environment": "production", "oauth_client_id": client, "registration_protocol": 2, "catalog_version": set.CatalogVersion, "coverage_digest": "", "image_digest": "", "raw_jws": "", "created_at": now}
		if err := db.Table("license_runtime_member").Create(member).Error; err != nil {
			t.Fatal(err)
		}
		check(i == 1)
	}
	if err := db.Table("license_runtime_member").Where("service_id = ?", "contract-worker").Update("registration_protocol", 1).Error; err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := db.Table("license_runtime_member").Where("service_id = ?", "contract-worker").Update("registration_protocol", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("platform_oauth_client").Where("client_id = ?", "registration-contract-worker").Update("status", "DISABLED").Error; err != nil {
		t.Fatal(err)
	}
	check(false)
	tracker.registrationImages = imageReaderStub{reported: false}
	if _, err := tracker.runtimeRegistrationMatches(context.Background(), tenant, "contract_management", "prod"); err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatal("unverified inventory accepted", err)
	}
}
