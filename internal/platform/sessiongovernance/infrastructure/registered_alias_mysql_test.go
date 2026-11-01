package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"gorm.io/gorm"
)

// TestRegisteredKeycloakAliasRedirectMySQL 验证映射别名仅能复用同应用环境的已登记网页回调。
// 参数：t为真实全迁移隔离MySQL测试上下文。
// 返回值：无；合法别名拒绝、边界扩张或数据库配置变更即失败。
func TestRegisteredKeycloakAliasRedirectMySQL(t *testing.T) {
	store := openSessionTestStore(t)
	tx := store.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	store.db = tx
	seedRegisteredAlias(t, tx)
	callback := "https://portal.example/customer-portal/auth/callback"
	for _, client := range []string{"central_alias_qa-prod-web", "keycloak-session-alias-qa"} {
		t.Run(client, func(t *testing.T) {
			got, err := store.RegisteredClient(context.Background(), client, callback)
			if err != nil || got.ID != client || got.ApplicationCode != "central_alias_qa" || got.EnvironmentCode != "prod" {
				t.Errorf("registered alias resolution failed: client=%s result=%+v error=%v", client, got, err)
			}
		})
	}
	for _, target := range []string{"https://attacker.example/callback", callback + "/", "https://other.example/callback"} {
		if _, err := store.RegisteredClient(context.Background(), "keycloak-session-alias-qa", target); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("unregistered callback accepted: %s error=%v", target, err)
		}
	}
	for _, table := range []string{"keycloak_application_client_mapping", "platform_oauth_client", "platform_application_environment", "platform_application"} {
		t.Run("inactive-"+table, func(t *testing.T) {
			id := "app-session-alias-qa"
			predicate := "id = ?"
			if table == "keycloak_application_client_mapping" {
				predicate = "application_id = ?"
			} else if table == "platform_oauth_client" {
				id = "client-session-web-qa"
			} else if table == "platform_application_environment" {
				id = "env-session-alias-qa"
			}
			previous := "ACTIVE"
			if table == "keycloak_application_client_mapping" {
				previous = "SYNCED"
			}
			status := "DISABLED"
			if previous == "SYNCED" {
				status = "FAILED"
			}
			if err := tx.Table(table).Where(predicate, id).Update("status", status).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Table(table).Where(predicate, id).Update("status", previous).Error })
			if _, err := store.RegisteredClient(context.Background(), "keycloak-session-alias-qa", callback); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("inactive boundary accepted: %s error=%v", table, err)
			}
		})
	}
}

// seedRegisteredAlias 按真实约束准备标准网页客户端与既有Keycloak别名的最小合成投影。
// 参数：t为测试上下文，db为仅用于本用例且最终回滚的隔离事务。
// 返回值：无；夹具无法满足真实迁移约束即失败。
func seedRegisteredAlias(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	rows := []struct {
		table string
		value map[string]any
	}{
		{"platform_application", map[string]any{
			"id": "app-session-alias-qa", "tenant_id": "tenant-session-qa",
			"code": "central_alias_qa", "name": "alias QA", "application_type": "BUSINESS",
			"status": "ACTIVE", "created_at": now, "updated_at": now,
		}},
		{"platform_application_environment", map[string]any{
			"id": "env-session-alias-qa", "tenant_id": "tenant-session-qa",
			"application_id": "app-session-alias-qa", "environment": "prod",
			"status": "ACTIVE", "created_at": now, "updated_at": now,
		}},
		{"platform_application_environment", map[string]any{
			"id": "env-session-other-qa", "tenant_id": "tenant-session-qa",
			"application_id": "app-session-alias-qa", "environment": "test",
			"status": "ACTIVE", "created_at": now, "updated_at": now,
		}},
	}
	for _, row := range rows {
		if err := db.Table(row.table).Create(row.value).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, client := range []struct{ id, name, environmentID, redirect string }{
		{"client-session-web-qa", "central_alias_qa-prod-web", "env-session-alias-qa",
			"https://portal.example/customer-portal/auth/callback"},
		{"client-session-broker-qa", "keycloak-session-alias-qa", "env-session-alias-qa",
			"https://idp.example/realms/qa/broker/customer/endpoint"},
		{"client-session-other-qa", "central_alias_qa-test-web", "env-session-other-qa",
			"https://other.example/callback"},
	} {
		value := map[string]any{
			"id": client.id, "tenant_id": "tenant-session-qa",
			"application_id": "app-session-alias-qa", "environment_id": client.environmentID,
			"client_id": client.name, "client_name": "alias QA", "client_type": "CONFIDENTIAL",
			"token_auth_method": "client_secret_basic", "access_token_ttl_seconds": 300,
			"refresh_token_ttl_seconds": 28800, "status": "ACTIVE",
			"created_at": now, "updated_at": now,
		}
		if err := db.Table("platform_oauth_client").Create(value).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("platform_oauth_redirect_uri").Create(map[string]any{"oauth_client_id": client.id, "redirect_uri": client.redirect, "created_at": now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("platform_oauth_grant_type").Create(map[string]any{"oauth_client_id": client.id, "grant_type": "authorization_code"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mapping := map[string]any{
		"tenant_id": "tenant-session-qa", "application_id": "app-session-alias-qa",
		"environment_id": "env-session-alias-qa", "realm": "qa",
		"keycloak_client_id": "keycloak-session-alias-qa", "status": "SYNCED",
		"created_at": now, "updated_at": now,
	}
	if err := db.Table("keycloak_application_client_mapping").Create(mapping).Error; err != nil {
		t.Fatal(err)
	}
}
