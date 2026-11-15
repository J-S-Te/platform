package infrastructure

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/gorm"
)

// TestCurrentClientCredentialPriorityMySQL 验证新凭据优先且两种认证保留相同有效集合。
// 参数：t为测试上下文，仅沿用受高位回环端口保护的专用OIDC测试库。
// 返回值：无；排序漂移、过滤失效或停用客户端仍可读取时失败。
func TestCurrentClientCredentialPriorityMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	db := oidcSessionIdleMySQLDatabase(t, ctx)
	for _, name := range []string{"000005_create_oauth_clients.sql", "000020_create_identity_enhancements.sql"} {
		ddl, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(ddl)).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newOIDCSessionIdleFixture(t, db, now, -time.Minute, nil)
	id := fixture.tenantID
	seedPriorityClient(t, db, id, now)
	seedPriorityCredentials(t, db, id, now)
	assertPriorityCredentials(t, db, id, now, []string{"new-b", "new-a", "old"})
	assertPriorityCredentials(t, db, id, now.Add(time.Hour), []string{"future", "new-b", "new-a"})
	if err := db.Table("platform_oauth_client_credential").Where("oauth_client_id=?", id).
		Updates(map[string]any{"revoked_at": now, "status": "REVOKED"}).Error; err != nil {
		t.Fatal(err)
	}
	assertPriorityCredentials(t, db, id, now, nil)
	if err := db.Table("platform_oauth_client").Where("id=?", id).Update("status", "DISABLED").Error; err != nil {
		t.Fatal(err)
	}
	oidcRepo, _ := NewRepository(db)
	registryRepo, _ := registry.NewRepository(db)
	if _, err := oidcRepo.FindClient(ctx, id, now); err == nil {
		t.Fatal("disabled OIDC client accepted")
	}
	if _, _, err := registryRepo.FindForClientCredentials(ctx, id, now); err == nil {
		t.Fatal("disabled machine client accepted")
	}
}

// seedPriorityClient 建立本测试独有的应用、环境和客户端并精确登记清理。
// 参数：t为测试上下文，db为隔离连接，id为本测试租户键，now为固定时刻。
// 返回值：无；元数据写入失败时终止测试，不触碰其他测试行。
func seedPriorityClient(t *testing.T, db *gorm.DB, id string, now time.Time) {
	t.Helper()
	t.Cleanup(func() {
		for _, table := range []string{"platform_oauth_client_credential", "platform_oauth_client",
			"platform_application_environment", "platform_application"} {
			key := "id"
			if table == "platform_oauth_client_credential" {
				key = "oauth_client_id"
			}
			if err := db.Table(table).Where(key+"=?", id).Delete(map[string]any{}).Error; err != nil {
				t.Error("clean priority fixture:", err)
			}
		}
	})
	rows := []struct {
		table string
		row   map[string]any
	}{
		{"platform_application", map[string]any{"code": id, "name": "priority-fixture", "application_type": "backend"}},
		{"platform_application_environment", map[string]any{"application_id": id, "environment": "test"}},
		{"platform_oauth_client", map[string]any{"application_id": id, "environment_id": id,
			"client_id": id, "client_name": "priority-fixture", "client_type": "confidential",
			"token_auth_method": "client_secret_basic", "access_token_ttl_seconds": 900,
			"refresh_token_ttl_seconds": 900}},
	}
	for _, seed := range rows {
		seed.row["id"], seed.row["tenant_id"], seed.row["status"] = id, id, "ACTIVE"
		seed.row["created_at"], seed.row["updated_at"] = now, now
		if err := db.Table(seed.table).Create(seed.row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// seedPriorityCredentials 建立时间并列、重叠窗口及不可用凭据候选。
// 参数：t为测试上下文，db为隔离连接，id为本测试客户端键，now为固定时刻。
// 返回值：无；所有hash仅为辨认排序的合成值，不参与密码性能基准。
func seedPriorityCredentials(t *testing.T, db *gorm.DB, id string, now time.Time) {
	t.Helper()
	for i, seed := range []struct {
		hash string
		from time.Time
		end  *time.Time
		gone *time.Time
	}{
		{"old", now.Add(-3 * time.Hour), pointerTo(now.Add(time.Hour)), nil},
		{"new-a", now.Add(-time.Hour), nil, nil},
		{"new-b", now.Add(-time.Hour), nil, nil},
		{"expired", now.Add(-time.Hour), &now, nil},
		{"future", now.Add(time.Hour), nil, nil},
		{"revoked", now.Add(-time.Hour), nil, &now},
	} {
		row := map[string]any{"id": fmt.Sprintf("priority-%017d", i), "oauth_client_id": id,
			"credential_type": "secret", "secret_hash": []byte(seed.hash), "status": "ACTIVE",
			"valid_from": seed.from, "valid_until": seed.end, "revoked_at": seed.gone, "created_at": now}
		if err := db.Table("platform_oauth_client_credential").Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// assertPriorityCredentials 对照两种真实仓储读取的完整有效候选与稳定次序。
// 参数：t为测试上下文，db为隔离连接，id为客户端键，now为判定时刻，want为合成hash次序。
// 返回值：无；任一实现漏掉轮换旧值、纳入失效凭据或次序变化时失败。
func assertPriorityCredentials(t *testing.T, db *gorm.DB, id string, now time.Time, want []string) {
	t.Helper()
	oidcRepo, _ := NewRepository(db)
	client, err := oidcRepo.FindClient(context.Background(), id, now)
	if err != nil {
		t.Fatal(err)
	}
	registryRepo, _ := registry.NewRepository(db)
	_, credentials, err := registryRepo.FindForClientCredentials(context.Background(), id, now)
	if err != nil {
		t.Fatal(err)
	}
	var oidcHashes, machineHashes []string
	for _, credential := range client.Credentials {
		oidcHashes = append(oidcHashes, string(credential.SecretHash))
	}
	for _, credential := range credentials {
		machineHashes = append(machineHashes, string(credential.SecretHash))
	}
	if !reflect.DeepEqual(oidcHashes, want) || !reflect.DeepEqual(machineHashes, want) {
		t.Fatalf("current credential order: oidc=%v machine=%v want=%v", oidcHashes, machineHashes, want)
	}
}
