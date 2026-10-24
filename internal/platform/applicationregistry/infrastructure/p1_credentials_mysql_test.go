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
