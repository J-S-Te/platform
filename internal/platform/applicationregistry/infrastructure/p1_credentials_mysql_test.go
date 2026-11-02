//go:build qa_p1_platform_db

package infrastructure

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestP1MySQLSecretRedeliveryCASOperatorAndExistingCredential(t *testing.T) {
	path := os.Getenv("UIP_BI_TEST_MYSQL_DSN_FILE")
	if path == "" {
		t.Fatal("dedicated private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("dedicated DSN file unavailable")
	}
	cfg, err := mysqlDriver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || cfg.DBName != "dashboard_p1_test_20261013" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("fixture refuses nonisolated schema/host")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open isolated fixture")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("fixture pool unavailable")
	}
	defer pool.Close()
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('platform_oauth_client','platform_oauth_client_credential')").Scan(&count).Error; err != nil || count != 0 {
		t.Fatal("credential fixture refuses existing tables")
	}
	if err := db.AutoMigrate(&oauthClientManagementModel{}, &oauthClientCredentialModel{}); err != nil {
		t.Fatal("credential fixture migration failed")
	}
	defer func() {
		if err := db.Migrator().DropTable(&oauthClientCredentialModel{}, &oauthClientManagementModel{}); err != nil {
			t.Error("owned credential fixture cleanup failed")
		}
	}()
	now := time.Now().UTC().Truncate(time.Millisecond)
	client := oauthClientManagementModel{ID: "p1-source-client", TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1", ClientID: "contract_management-prod-settlement-integration", ClientType: "service", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE", Version: 7, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal("fixture client seed failed")
	}
	if err := createOAuthClientSecret(db, client.ID, application.SecretWrite{CredentialID: "p1-prior-secret", SecretHash: []byte("fixture-hash"), Fingerprint: "fixture-old"}, now); err != nil {
		t.Fatal("fixture prior credential seed failed")
	}
	repository, _ := NewOAuthClientManagementRepository(db)
	expected := uint64(7)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, id := range []string{"p1-retry-secret-a", "p1-retry-secret-b"} {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			<-start
			_, err := repository.CreateOAuthClientSecret(context.Background(), application.OAuthClientSecretCreateInput{TenantID: "tenant-1", OAuthClientID: client.ID, OperatorID: "controlled-operator", ExpectedClientVersion: &expected}, application.SecretWrite{CredentialID: id, SecretHash: []byte("fixture-new-hash"), Fingerprint: "fixture-new"}, now)
			results <- err
		}(id)
	}
	close(start)
	wait.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, application.ErrManagementConflict) {
			conflict++
		} else {
			t.Fatal("unexpected secret redelivery failure")
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", success, conflict)
	}
	var updated oauthClientManagementModel
	if err := db.Where("id=?", client.ID).Take(&updated).Error; err != nil {
		t.Fatal("fixture metadata read failed")
	}
	if updated.Version != 8 || updated.UpdatedBy == nil || *updated.UpdatedBy != "controlled-operator" {
		t.Fatal("operator/version audit metadata missing")
	}
	var credentials []oauthClientCredentialModel
	if err := db.Where("oauth_client_id=?", client.ID).Find(&credentials).Error; err != nil {
		t.Fatal("credential metadata read failed")
	}
	if len(credentials) != 2 {
		t.Fatal("stale retry inserted credential or prior credential removed")
	}
	for _, credential := range credentials {
		if credential.Status != "ACTIVE" || credential.RevokedAt != nil || credential.ValidUntil != nil {
			t.Fatal("redelivery revoked/narrowed existing credential")
		}
	}
	_, err = repository.CreateOAuthClientSecret(context.Background(), application.OAuthClientSecretCreateInput{TenantID: "other-tenant", OAuthClientID: client.ID, OperatorID: "other", ExpectedClientVersion: &expected}, application.SecretWrite{CredentialID: "p1-wrong-tenant", SecretHash: []byte("fixture")}, now)
	if !errors.Is(err, application.ErrManagementNotFound) {
		t.Fatal("cross-tenant secret creation allowed")
	}
	t.Log("actual MySQL: concurrent owner CAS 1 success/1 conflict; operator/version recorded; old credential ACTIVE; cross tenant denied; owned tables cleaned")
}

// 旧 Secret 退役（RUNTIME-05）：只有显式开启策略才收紧既有凭据。
// 上面已有的 CAS 用例固定了默认 keep 行为（重试不吊销旧密钥）；这里验证开启后
// REVOKE 立即吊销旧版本、GRACE 只收紧有效期且不写 REVOKED，两者都与新版本在同一事务完成。
func TestP1MySQLCreateSecretRetiresPreviousCredentialWhenConfigured(t *testing.T) {
	path := os.Getenv("UIP_BI_TEST_MYSQL_DSN_FILE")
	if path == "" {
		t.Fatal("dedicated private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("dedicated DSN file unavailable")
	}
	cfg, err := mysqlDriver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || cfg.DBName != "dashboard_p1_test_20261013" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("fixture refuses nonisolated schema/host")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open isolated fixture")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("fixture pool unavailable")
	}
	defer pool.Close()
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('platform_oauth_client','platform_oauth_client_credential')").Scan(&count).Error; err != nil || count != 0 {
		t.Fatal("credential fixture refuses existing tables")
	}
	if err := db.AutoMigrate(&oauthClientManagementModel{}, &oauthClientCredentialModel{}); err != nil {
		t.Fatal("credential fixture migration failed")
	}
	defer func() {
		if err := db.Migrator().DropTable(&oauthClientCredentialModel{}, &oauthClientManagementModel{}); err != nil {
			t.Error("owned credential fixture cleanup failed")
		}
	}()
	now := time.Now().UTC().Truncate(time.Millisecond)
	client := oauthClientManagementModel{ID: "p1-retire-client", TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1", ClientID: "contract_management-prod-audit-publisher", ClientType: "service", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE", Version: 7, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal("fixture client seed failed")
	}
	if err := createOAuthClientSecret(db, client.ID, application.SecretWrite{CredentialID: "p1-prior-secret", SecretHash: []byte("fixture-hash"), Fingerprint: "fixture-prior"}, now); err != nil {
		t.Fatal("fixture prior credential seed failed")
	}
	repository, _ := NewOAuthClientManagementRepository(db)

	// REVOKE：旧版本立即吊销，吊销痕迹留在行内（status + revoked_at），不删除行。
	expected := uint64(7)
	if _, err := repository.CreateOAuthClientSecret(context.Background(), application.OAuthClientSecretCreateInput{
		TenantID: "tenant-1", OAuthClientID: client.ID, OperatorID: "controlled-operator",
		ExpectedClientVersion: &expected, PreviousSecretPolicy: application.OAuthClientSecretRetirementRevoke,
	}, application.SecretWrite{CredentialID: "p1-revoke-secret", SecretHash: []byte("fixture-new-hash"), Fingerprint: "fixture-revoke"}, now); err != nil {
		t.Fatal("revoke-policy secret creation failed")
	}
	assertCredential := func(credentialID, status string, revoked, expiring bool) oauthClientCredentialModel {
		t.Helper()
		var credential oauthClientCredentialModel
		if err := db.Where("id=? AND oauth_client_id=?", credentialID, client.ID).Take(&credential).Error; err != nil {
			t.Fatalf("read credential %s: %v", credentialID, err)
		}
		if credential.Status != status || (credential.RevokedAt != nil) != revoked || (credential.ValidUntil != nil) != expiring {
			t.Fatalf("credential %s status=%s revoked=%v valid_until=%v, want status=%s revoked=%v expiring=%v",
				credentialID, credential.Status, credential.RevokedAt, credential.ValidUntil, status, revoked, expiring)
		}
		return credential
	}
	assertCredential("p1-prior-secret", "REVOKED", true, false)
	assertCredential("p1-revoke-secret", "ACTIVE", false, false)

	// GRACE：再签发一次，上一版本只收紧有效期到宽限窗口，不立即吊销。
	expected = 8
	grace := 15 * time.Minute
	if _, err := repository.CreateOAuthClientSecret(context.Background(), application.OAuthClientSecretCreateInput{
		TenantID: "tenant-1", OAuthClientID: client.ID, OperatorID: "controlled-operator",
		ExpectedClientVersion: &expected, PreviousSecretPolicy: application.OAuthClientSecretRetirementGrace, PreviousSecretGrace: grace,
	}, application.SecretWrite{CredentialID: "p1-grace-secret", SecretHash: []byte("fixture-grace-hash"), Fingerprint: "fixture-grace"}, now); err != nil {
		t.Fatal("grace-policy secret creation failed")
	}
	graced := assertCredential("p1-revoke-secret", "ACTIVE", false, true)
	if graced.ValidUntil == nil || !graced.ValidUntil.Equal(now.Add(grace)) {
		t.Fatalf("grace valid_until = %v, want %s", graced.ValidUntil, now.Add(grace))
	}
	assertCredential("p1-grace-secret", "ACTIVE", false, false)
	t.Log("actual MySQL: revoke policy revoked prior credential in-row; grace policy bounded prior validity without revoking; new credential active")
}
