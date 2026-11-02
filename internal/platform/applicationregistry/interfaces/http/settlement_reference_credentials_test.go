package http

import (
	"bytes"
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

// TestProjectReferenceMissingSourceGuidance 验证项目缺少结算身份时的两种口径：
// 来源尚未接入不再阻断（结算后接入），来源已登记但漂移仍返回受控接入指引。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectReferenceMissingSourceGuidance(t *testing.T) {
	handler := &SubsystemOnboardingHandler{service: &stubSubsystemOnboardingService{},
		serviceCredentials: &serviceCredentialManagerStub{},
		logger: slog.New(slog.NewTextHandler(io.Discard,
			nil))}
	clients, err := handler.relatedServiceClients(context.Background(), "tenant", "project_management", "prod")
	if err != nil || clients != nil {
		t.Fatalf("missing settlement source must be optional: clients=%v err=%v", clients, err)
	}
	handler.serviceCredentials = &serviceCredentialManagerStub{clients: []application.OAuthClientView{{ID: "reference-uid",
		TenantID:        "tenant",
		ApplicationID:   "app-1",
		EnvironmentID:   "env-1",
		ClientID:        "settlement-prod-project-reference",
		Status:          "ACTIVE",
		ClientType:      "service",
		TokenAuthMethod: "client_secret_basic",
		GrantTypes:      []string{"client_credentials"},
		// 已登记来源的 scope 漂移：必须失败并给出受控接入指引，不能静默关闭。
		Scopes: []string{"settlement.contract.ingest"}}}}
	_, err = handler.relatedServiceClients(context.Background(), "tenant", "project_management", "prod")
	if err == nil {
		t.Fatal("drifted source accepted")
	}
	response := httptest.NewRecorder()
	handler.writeError(response, httptest.NewRequest("POST", "/api/v1/subsystem-update", nil), err)
	if response.Code != 409 || !strings.Contains(response.Body.String(),
		"IAM_SUBSYSTEM_SETTLEMENT_REFERENCE_DEPENDENCY_REQUIRED") || !strings.Contains(response.Body.String(),
		"settlement_project_reference_read") {
		t.Fatal("wrong source preparation guidance")
	}
}

// TestProjectReferenceMissingSourceLogsIntegrationDisabled 验证项目先接入时平台只告警不阻断：
// 结算来源未登记时返回空集合，并记录"结算引用未就绪、集成已关闭"的平台告警。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectReferenceMissingSourceLogsIntegrationDisabled(t *testing.T) {
	var logs bytes.Buffer
	handler := &SubsystemOnboardingHandler{service: &stubSubsystemOnboardingService{},
		serviceCredentials: &serviceCredentialManagerStub{},
		logger:             slog.New(slog.NewTextHandler(&logs, nil))}
	clients, err := handler.relatedServiceClients(context.Background(), "tenant", "project_management", "prod")
	if err != nil || clients != nil {
		t.Fatalf("clients=%v err=%v", clients, err)
	}
	if !strings.Contains(logs.String(), "project settlement reference dependency not ready; integration disabled") {
		t.Fatalf("missing integration-disabled warning: %s", logs.String())
	}
	// 结算应用缺合同投递来源仍必须失败，且不会产生"结算引用未就绪"的告警。
	logs.Reset()
	if _, err := handler.relatedServiceClients(context.Background(), "tenant", "settlement", "prod"); err == nil {
		t.Fatal("missing settlement contract dependency accepted")
	}
	if strings.Contains(logs.String(), "project settlement reference dependency not ready") {
		t.Fatalf("unexpected project warning for settlement: %s", logs.String())
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
