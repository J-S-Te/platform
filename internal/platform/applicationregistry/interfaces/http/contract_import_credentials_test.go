package http

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"testing"
)

// TestImportReportCredentialDriftRejectsBeforeMutation 验证报告身份漂移在scope修复及密钥签发前拒绝。
// 参数：t为测试上下文，使用现有内存机器客户端管理替身。
// 返回值：无；漂移凭据发生任何目标scope或密钥修改即失败。
func TestImportReportCredentialDriftRejectsBeforeMutation(t *testing.T) {
	for _, tc := range []struct{ app, suffix string }{{"contract_management", "analysis-import-report-status"}, {"data_analysis", "contract-import-report-read"}} {
		for _, field := range []string{"tenant", "type", "auth", "grant"} {
			t.Run(tc.app+"/"+field, func(t *testing.T) {
				client := application.OAuthClientView{ID: "report-client", TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ClientID: tc.app + "-prod-" + tc.suffix, Status: "ACTIVE", Version: 7, ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: []string{"wrong-scope"}}
				switch field {
				case "tenant":
					client.TenantID = "other"
				case "type":
					client.ClientType = "confidential"
				case "auth":
					client.TokenAuthMethod = "none"
				case "grant":
					client.GrantTypes = []string{"authorization_code"}
				}
				manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{client}}
				handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
				_, err := handler.ensureUpdateServiceCredentials(context.Background(), "tenant", "app", "env", tc.app, "prod", "operator", "UPDATE")
				if err != application.ErrConflict {
					t.Fatalf("drift error=%v", err)
				}
				if len(manager.secretInputs) > 0 || len(manager.scopeInputs) > 0 {
					t.Fatal("drifted report identity mutated before rejection")
				}
			})
		}
	}
}

// TestImportReportCredentialUpdatePinsVersion 验证正常受控更新重新交付报告密钥并带版本围栏。
// 参数：t为测试上下文，使用合法同租户应用环境身份。
// 返回值：无；scope修复后签发不带最新版本或复用普通凭据即失败。
func TestImportReportCredentialUpdatePinsVersion(t *testing.T) {
	for _, tc := range []struct{ app, suffix, scope string }{{"contract_management", "analysis-import-report-status", "analysis.contract-import.report-status"}, {"data_analysis", "contract-import-report-read", "contract.import.report.read"}} {
		t.Run(tc.app, func(t *testing.T) {
			client := application.OAuthClientView{ID: "report-client", TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ClientID: tc.app + "-prod-" + tc.suffix, Status: "ACTIVE", Version: 7, ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: []string{"wrong-scope"}}
			manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{client}}
			handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
			_, err := handler.ensureUpdateServiceCredentials(context.Background(), "tenant", "app", "env", tc.app, "prod", "operator", "UPDATE")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, input := range manager.secretInputs {
				if input.OAuthClientID == client.ID {
					found = true
					if input.ExpectedClientVersion == nil || *input.ExpectedClientVersion != 8 || input.OperatorID != "operator" {
						t.Fatal("report secret lacks latest version fence")
					}
				}
			}
			if !found || len(manager.scopeInputs) != 1 || len(manager.scopeInputs[0].Scopes) != 1 || manager.scopeInputs[0].Scopes[0] != tc.scope {
				t.Fatal("report secret delivery or scope is wrong")
			}
		})
	}
}
