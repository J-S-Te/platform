package application

import (
	"testing"
	"time"
)

func TestP1BusinessCompletionPurposesCreateSeparateSingleScopeClients(t *testing.T) {
	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		app              string
		purposes, scopes []string
	}{
		{"contract_management", []string{"settlement_contract_ingest"}, []string{"settlement.contract.ingest"}},
		{"customer_portal", []string{"portal_report_file_read", "portal_report_download_identity_read"}, []string{"platform:file:portal_report", "portal.report.download_identity.read"}},
	} {
		t.Run(tc.app, func(t *testing.T) {
			writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: tc.app, ApplicationName: tc.app, Environment: "prod", AllowedServiceBindings: tc.purposes}, "application", "environment", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for i, purpose := range tc.purposes {
				var match *SubsystemServiceClientWrite
				for index := range writes {
					if writes[index].Purpose == purpose {
						match = &writes[index]
					}
				}
				if match == nil || len(match.OAuthClient.Scopes) != 1 || match.OAuthClient.Scopes[0] != tc.scopes[i] || len(match.OAuthClient.GrantTypes) != 1 || match.OAuthClient.GrantTypes[0] != "client_credentials" {
					t.Fatalf("missing or broad client for %s", purpose)
				}
				if secrets[purpose] == "" || seen[secrets[purpose]] {
					t.Fatalf("secret reused or absent for %s", purpose)
				}
				seen[secrets[purpose]] = true
			}
		})
	}
}
