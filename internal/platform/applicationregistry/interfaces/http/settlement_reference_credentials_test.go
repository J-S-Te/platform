package http

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSettlementReferenceUpdateRedeliversSecret 验证更新、接管和重试补齐并重新下发只写密钥。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementReferenceUpdateRedeliversSecret(t *testing.T) {
	for _, operation := range []string{"UPDATE", "ADOPT", "RETRY"} {
		t.Run(operation, func(t *testing.T) {
			manager := &serviceCredentialManagerStub{}
			handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
			credentials,
				err := handler.ensureUpdateServiceCredentials(context.Background(),
				"tenant-1",
				"app-1",
				"env-1",
				"settlement",
				"prod",
				"operator",
				operation)
			if err != nil {
				t.Fatal(err)
			}
			requireCreatedClient(t,
				manager.createdInputs,
				"settlement-prod-project-reference",
				[]string{"settlement.project_reference.read"})
			requireDeliveredCredential(t, credentials, "settlement_project_reference_read", "new-secret")
			manager = &serviceCredentialManagerStub{clients: []application.OAuthClientView{{ID: "reference-uid",
				TenantID:        "tenant-1",
				ApplicationID:   "app-1",
				EnvironmentID:   "env-1",
				ClientID:        "settlement-prod-project-reference",
				Status:          "ACTIVE",
				ClientType:      "service",
				TokenAuthMethod: "client_secret_basic",
				GrantTypes:      []string{"client_credentials"},
				Scopes:          []string{"settlement.project_reference.read"},
				Version:         3}}}
			handler.serviceCredentials = manager
			credentials,
				err = handler.ensureUpdateServiceCredentials(context.Background(),
				"tenant-1",
				"app-1",
				"env-1",
				"settlement",
				"prod",
				"operator",
				operation)
			if err != nil {
				t.Fatal(err)
			}
			requireDeliveredCredential(t, credentials, "settlement_project_reference_read", "retry-secret")
		})
	}
}

// TestProjectReferenceMissingSourceGuidance 验证项目缺少结算身份时返回正确受控接入指引。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectReferenceMissingSourceGuidance(t *testing.T) {
	handler := &SubsystemOnboardingHandler{service: &stubSubsystemOnboardingService{},
		serviceCredentials: &serviceCredentialManagerStub{},
		logger: slog.New(slog.NewTextHandler(io.Discard,
			nil))}
	_, err := handler.relatedServiceClients(context.Background(), "tenant", "project_management", "prod")
	if err == nil {
		t.Fatal("missing source accepted")
	}
	response := httptest.NewRecorder()
	handler.writeError(response, httptest.NewRequest("POST", "/api/v1/subsystem-update", nil), err)
	if response.Code != 409 || !strings.Contains(response.Body.String(),
		"IAM_SUBSYSTEM_SETTLEMENT_REFERENCE_DEPENDENCY_REQUIRED") || !strings.Contains(response.Body.String(),
		"settlement_project_reference_read") {
		t.Fatal("wrong source preparation guidance")
	}
}

// TestSettlementPlatformCredentialUpdateRejectsForeignIdentity 验证旧更新入口拒绝同名错误机器身份。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementPlatformCredentialUpdateRejectsForeignIdentity(t *testing.T) {
	for suffix, scope := range map[string]string{
		"owner-directory": "owner_directory.read", "notification-publisher": "notification.ingest",
		"audit-publisher": "audit.ingest",
	} {
		for _, fault := range []string{"tenant", "app", "environment", "type", "auth", "grant"} {
			t.Run(suffix+"/"+fault, func(t *testing.T) {
				client := application.OAuthClientView{
					ID: "uid", TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1",
					ClientID: "settlement-prod-" + suffix, Status: "ACTIVE", ClientType: "service",
					TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
					Scopes: []string{scope}, Version: 1,
				}
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
				manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{client}}
				handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
				_, err := handler.ensureUpdateServiceCredentials(context.Background(), "tenant-1", "app-1", "env-1", "settlement", "prod", "operator", "UPDATE")
				if err == nil || len(manager.secretInputs) != 0 {
					t.Fatal("foreign identity rotated or delivered")
				}
			})
		}
	}
}
