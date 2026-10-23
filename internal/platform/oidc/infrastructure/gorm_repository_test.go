package infrastructure

import (
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	"gorm.io/gorm/schema"
)

// TestOAuthClientIDColumnMappings 保护含大量缩写的 OAuth 字段，防止 GORM 默认
// 转换为不存在的 o_auth_client_id 列。数据库 schema 在授权码、令牌家族、
// 刷新令牌、撤销和 PAR 中统一使用 oauth_client_id。
func TestOAuthClientIDColumnMappings(t *testing.T) {
	t.Parallel()

	models := []struct {
		name  string
		value any
	}{
		{name: "authorization code", value: authorizationCodeRow{}},
		{name: "token family", value: tokenFamilyRow{}},
		{name: "refresh token", value: refreshTokenRow{}},
		{name: "refresh projection", value: refreshProjection{}},
		{name: "token revocation", value: tokenRevocationRow{}},
		{name: "userinfo projection", value: userInfoProjection{}},
		{name: "pushed authorization request", value: parRow{}},
	}

	for _, model := range models {
		model := model
		t.Run(model.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := schema.Parse(model.value, &sync.Map{}, schema.NamingStrategy{})
			if err != nil {
				t.Fatalf("parse GORM schema: %v", err)
			}
			field := parsed.LookUpField("OAuthClientID")
			if field == nil {
				t.Fatal("OAuthClientID field is missing")
			}
			if field.DBName != "oauth_client_id" {
				t.Fatalf("OAuthClientID column = %q, want oauth_client_id", field.DBName)
			}
		})
	}
}

func TestRefreshProjectionRejectsInactiveTokenFamily(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	familyRevokedAt := now.Add(-time.Minute)
	token := refreshFromProjection(refreshProjection{
		ID: "refresh-1", Status: domain.RefreshTokenStatusActive, ExpiresAt: now.Add(time.Hour),
		FamilyStatus: domain.TokenFamilyStatusRevoked, FamilyRevokedAt: &familyRevokedAt,
		FamilyExpiresAt: now.Add(time.Hour),
	}, now)

	if token.Status != domain.RefreshTokenStatusRevoked || token.RevokedAt == nil || !token.RevokedAt.Equal(familyRevokedAt) {
		t.Fatalf("refresh token = %#v", token)
	}
}
