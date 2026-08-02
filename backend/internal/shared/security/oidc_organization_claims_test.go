package security

import (
	"fmt"
	"testing"
	"time"
)

func TestValidateOIDCTokenClaimsRequiresCanonicalOrganizations(t *testing.T) {
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	base := OIDCTokenClaims{
		Issuer: "https://identity.example", Subject: "user-1", Audience: []string{"crm-client"},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), JWTID: "token-1", SessionID: "session-1",
		AuthenticationTime: now, Scope: []string{"openid"}, ClientID: "crm-client",
		TokenUse: OIDCTokenUseIDToken, TenantID: "tenant-1",
		PrimaryOrgID: "org-b", OrganizationIDs: []string{"org-a", "org-b"},
	}
	if err := validateOIDCTokenClaims(base, base.Issuer, OIDCTokenUseIDToken); err != nil {
		t.Fatalf("valid organization claims rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*OIDCTokenClaims)
	}{
		{"empty tenant", func(value *OIDCTokenClaims) { value.TenantID = "" }},
		{"non-canonical tenant", func(value *OIDCTokenClaims) { value.TenantID = " tenant-1" }},
		{"person whitespace", func(value *OIDCTokenClaims) { value.PersonID = " PMS-A" }},
		{"person control", func(value *OIDCTokenClaims) { value.PersonID = "PMS\nA" }},
		{"person unicode incompatible with ASCII schema", func(value *OIDCTokenClaims) { value.PersonID = "人员-一" }},
		{"person invisible format character", func(value *OIDCTokenClaims) { value.PersonID = "PMS-\u200bA" }},
		{"unsorted", func(value *OIDCTokenClaims) { value.OrganizationIDs = []string{"org-b", "org-a"} }},
		{"duplicate", func(value *OIDCTokenClaims) { value.OrganizationIDs = []string{"org-a", "org-a"} }},
		{"primary outside set", func(value *OIDCTokenClaims) { value.PrimaryOrgID = "org-c" }},
		{"too many", func(value *OIDCTokenClaims) {
			value.PrimaryOrgID = ""
			value.OrganizationIDs = make([]string, maxOIDCOrganizationIDs+1)
			for index := range value.OrganizationIDs {
				value.OrganizationIDs[index] = organizationTestID(index)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.OrganizationIDs = append([]string(nil), base.OrganizationIDs...)
			test.mutate(&value)
			if err := validateOIDCTokenClaims(value, value.Issuer, OIDCTokenUseIDToken); err == nil {
				t.Fatal("invalid organization claims accepted")
			}
		})
	}
}

func organizationTestID(index int) string {
	return fmt.Sprintf("org-%03d", index)
}
