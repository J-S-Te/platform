package infrastructure

import (
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm/clause"
)

// 旧 Secret 退役的 SQL 语义（RUNTIME-05）：REVOKE 必须同时写 REVOKED 与 revoked_at，
// GRACE 只能收紧 valid_until（不写状态、不延长更早的截止时间），未知策略拒绝执行。
// 这三个契约在仓储事务内被直接执行，因此在此以纯函数固定，避免回归成“只追加”。
func TestOAuthClientSecretRetirementUpdates(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	revoke, err := oauthClientSecretRetirementUpdates(application.OAuthClientSecretRetirementRevoke, 0, now)
	if err != nil {
		t.Fatalf("revoke updates: %v", err)
	}
	if revoke["status"] != "REVOKED" {
		t.Fatalf("revoke status = %v, want REVOKED", revoke["status"])
	}
	if revokedAt, ok := revoke["revoked_at"].(time.Time); !ok || !revokedAt.Equal(now) {
		t.Fatalf("revoke revoked_at = %v, want %s", revoke["revoked_at"], now)
	}
	if _, exists := revoke["valid_until"]; exists {
		t.Fatal("revoked credentials must not be revived by a validity window")
	}

	grace, err := oauthClientSecretRetirementUpdates(application.OAuthClientSecretRetirementGrace, 30*time.Minute, now)
	if err != nil {
		t.Fatalf("grace updates: %v", err)
	}
	if _, exists := grace["status"]; exists {
		t.Fatal("grace policy must only shorten validity; it must not revoke immediately")
	}
	expr, ok := grace["valid_until"].(clause.Expr)
	if !ok {
		t.Fatalf("grace valid_until = %T, want clause.Expr", grace["valid_until"])
	}
	if expr.SQL != "CASE WHEN valid_until IS NULL OR valid_until > ? THEN ? ELSE valid_until END" {
		t.Fatalf("grace validity must never extend an earlier deadline, got %q", expr.SQL)
	}
	if len(expr.Vars) != 2 || expr.Vars[0] != now.Add(30*time.Minute) || expr.Vars[1] != now.Add(30*time.Minute) {
		t.Fatalf("grace vars = %v, want the bounded grace window twice", expr.Vars)
	}

	for _, policy := range []application.OAuthClientSecretRetirementPolicy{application.OAuthClientSecretRetirementKeep, application.OAuthClientSecretRetirementPolicy("ALWAYS")} {
		if _, err := oauthClientSecretRetirementUpdates(policy, 0, now); !errors.Is(err, application.ErrManagementValidation) {
			t.Fatalf("policy %q must be rejected by the SQL builder, err=%v", policy, err)
		}
	}
	if _, err := oauthClientSecretRetirementUpdates(application.OAuthClientSecretRetirementGrace, 0, now); !errors.Is(err, application.ErrManagementValidation) {
		t.Fatalf("grace without a window must be rejected, err=%v", err)
	}
}
