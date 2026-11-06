package http

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestSettlementRelatedClientHandlerUsesOnlyAgentProjection 验证正式HTTP共享helper只接受真实Agent模式。
// 参数：t为测试上下文，目录、凭据和Agent均使用已存在的受控元数据替身。
// 返回值：无；缺模式、错环境、坏投影、读取失败或登记漂移不能降级为成功。
func TestSettlementRelatedClientHandlerUsesOnlyAgentProjection(t *testing.T) {
	for _, mode := range []string{"native", "legacy", "wrong_environment", "duplicate", "agent_failure", "drifted", "context_claim"} {
		t.Run(mode, func(t *testing.T) {
			caps := application.SubsystemProvisioningCapabilities{Enabled: true, Mode: "production",
				SupportedApplicationCodes: []string{"settlement"}, SupportedEnvironments: []string{"prod"},
				Targets:                                []application.SubsystemProvisioningTarget{{ApplicationCode: "settlement", Environment: "prod"}},
				SettlementContractOptionalEnvironments: []string{"prod"}}
			provider := &liveCapabilityHTTPSubsystemProvisioner{recordingHTTPSubsystemProvisioner: &recordingHTTPSubsystemProvisioner{}, available: caps}
			manager := &serviceCredentialManagerStub{}
			environment := "prod"
			switch mode {
			case "legacy", "context_claim":
				provider.available.SettlementContractOptionalEnvironments = nil
			case "wrong_environment":
				environment = "dev"
			case "duplicate":
				provider.available.SettlementContractOptionalEnvironments = []string{"prod", "prod"}
			case "agent_failure":
				provider.availableErr = errors.New("Agent unavailable")
			case "drifted":
				manager.clients = []application.OAuthClientView{{ID: "source-client", TenantID: "tenant", ApplicationID: "app-1", EnvironmentID: "env-1",
					ClientID: "contract_management-prod-settlement-integration", Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: []string{"*"}}}
			}
			handler := &SubsystemOnboardingHandler{service: &stubSubsystemOnboardingService{}, serviceCredentials: manager, provisioner: provider}
			ctx := context.Background()
			if mode == "context_claim" {
				ctx = context.WithValue(ctx, struct{}{}, "SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true")
			}
			clients, err := handler.relatedServiceClients(ctx, "tenant", "settlement", environment)
			if mode == "native" {
				if err != nil || len(clients) != 0 {
					t.Fatalf("Agent native projection rejected: %+v %v", clients, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("unsafe related-client policy accepted: %s", mode)
			}
		})
	}
}
