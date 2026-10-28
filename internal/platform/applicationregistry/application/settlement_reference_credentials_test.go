package application

import (
	"context"
	"testing"
	"time"
)

// TestSettlementReferenceInitialClients 验证首次接入创建独立单scope机器身份。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementReferenceInitialClients(t *testing.T) {
	service,
		err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{},
		&sequentialManagementIDs{},
		fixedSubsystemClock{now: time.Now()},
		RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	writes,
		secrets,
		err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant",
		OperatorID:      "operator",
		ApplicationCode: "settlement",
		ApplicationName: "结算",
		Environment:     "prod",
		AllowedServiceBindings: []string{"settlement_project_reference_read",
			"owner_directory_read",
			"notification_ingest"}},
		"app",
		"env",
		time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{"settlement_project_reference_read",
		"owner_directory_read",
		"notification_ingest",
		"audit_ingest"} {
		found := false
		for _, write := range writes {
			if write.Purpose != purpose {
				continue
			}
			found = true
			if write.OAuthClient.ClientType != "service" || len(write.OAuthClient.Scopes) != 1 || write.OAuthClient.ApplicationID != "app" || write.OAuthClient.EnvironmentID != "env" || secrets[purpose] == "" {
				t.Fatal("invalid independent service client")
			}
			if purpose == "settlement_project_reference_read" && write.OAuthClient.Scopes[0] != "settlement.project_reference.read" {
				t.Fatal("wrong project scope")
			}
			for other, secret := range secrets {
				if other != purpose && secret == secrets[purpose] {
					t.Fatal("secret reused")
				}
			}
		}
		if !found {
			t.Fatalf("missing purpose %s", purpose)
		}
	}
}

// TestSettlementReferenceRelatedClientResolver 验证项目入站从真实登记元数据解析及拒绝漂移。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementReferenceRelatedClientResolver(t *testing.T) {
	for _, name := range []string{"valid", "missing", "tenant", "app", "environment", "scope", "type", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			client := canonicalRelatedClient()
			client.ClientID = "settlement-prod-project-reference"
			client.Scopes = []string{"settlement.project_reference.read"}
			switch name {
			case "tenant":
				client.TenantID = "other"
			case "app":
				client.ApplicationID = "other"
			case "environment":
				client.EnvironmentID = "other"
			case "scope":
				client.Scopes = append(client.Scopes, "dashboard.project.read")
			case "type":
				client.ClientType = "confidential"
			}
			clients := []OAuthClientView{client}
			if name == "missing" {
				clients = nil
			}
			if name == "duplicate" {
				clients = append(clients, client)
			}
			registry := &relatedRegistryStub{applicationID: "01APP000000000000000000002",
				environmentID: "01ENV000000000000000000001"}
			result,
				err := relatedResolverFor(t,
				registry,
				relatedClientListerStub{clients: clients}).Resolve(context.Background(),
				"tenant-1",
				"project_management",
				"prod")
			if name != "valid" {
				if err == nil {
					t.Fatal("untrusted source accepted")
				}
				return
			}
			if err != nil || len(result) != 1 || result[0].Purpose != "settlement_project_reference_read" || result[0].ApplicationCode != "settlement" {
				t.Fatalf("project binding unavailable: %v", err)
			}
		})
	}
}

// TestSettlementPlatformCredentialServiceRejectsForeignIdentity 验证统一更新服务拒绝结算三用途的错归属。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementPlatformCredentialServiceRejectsForeignIdentity(t *testing.T) {
	for _, requirement := range SubsystemUpdateServiceCredentialRequirements("settlement") {
		if requirement.Purpose != "owner_directory_read" && requirement.Purpose != "notification_ingest" && requirement.Purpose != "audit_ingest" {
			continue
		}
		for _, fault := range []string{"tenant", "app", "environment", "type", "auth", "grant"} {
			t.Run(requirement.Purpose+"/"+fault, func(t *testing.T) {
				client := updateCredentialClient()
				client.ClientID = "settlement-dev-" + requirement.Suffix
				client.Scopes = requirement.Scopes
				switch fault {
				case "tenant":
					client.TenantID = "other"
				case "app":
					client.ApplicationID = "other"
				case "environment":
					client.EnvironmentID = "other"
				case "type":
					client.ClientType = "confidential"
				case "auth":
					client.TokenAuthMethod = "client_secret_post"
				case "grant":
					client.GrantTypes = []string{"authorization_code"}
				}
				port := &updateCredentialPortStub{clients: []OAuthClientView{client}}
				request := updateCredentialRequest("UPDATE", true)
				request.Requirements = []SubsystemUpdateServiceCredentialRequirement{requirement}
				_, err := updateCredentialServiceFor(t, port).Ensure(context.Background(), request)
				if err == nil || port.secretCalls != 0 {
					t.Fatal("foreign platform identity rotated or delivered")
				}
			})
		}
	}
}
