package infrastructure

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// TestOAuthClientIDColumnMappings protects acronym-heavy OAuth fields from GORM's default
// conversion to the nonexistent o_auth_client_id column. The database schema consistently uses
// oauth_client_id for authorization codes, token families, refresh tokens, revocations, and PAR.
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
