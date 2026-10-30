package application

import (
	"testing"
	"time"
)

// TestContractImportCredentialsSeparateScopes 验证历史报告两方向为独立单scope机器身份。
// 参数：t为测试上下文，使用内存登记仓储，不调用真实身份平台。
// 返回值：无；用途未登记、scope扩张或密钥复用即失败。
func TestContractImportCredentialsSeparateScopes(t *testing.T) {
	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ app, purpose, scope, suffix string }{
		{"contract_management", "analysis_contract_import_report_status", "analysis.contract-import.report-status", "analysis-import-report-status"},
		{"data_analysis", "contract_import_report_read", "contract.import.report.read", "contract-import-report-read"},
	} {
		t.Run(tc.app, func(t *testing.T) {
			writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: tc.app, ApplicationName: tc.app, Environment: "prod", AllowedServiceBindings: []string{tc.purpose}}, "app", "env", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, write := range writes {
				if write.Purpose == tc.purpose {
					found = true
					if len(write.OAuthClient.Scopes) != 1 || write.OAuthClient.Scopes[0] != tc.scope || write.OAuthClient.ClientID != tc.app+"-prod-"+tc.suffix {
						t.Fatalf("invalid credential identity for %s", tc.purpose)
					}
					if secrets[tc.purpose] == "" || secrets[tc.purpose] == secrets[ServiceCredentialAuditIngest] {
						t.Fatal("new credential missing or reused")
					}
				}
			}
			if !found {
				t.Fatal("missing registered purpose")
			}
			requirement := false
			for _, item := range SubsystemUpdateServiceCredentialRequirements(tc.app) {
				if item.Purpose == tc.purpose {
					requirement = item.Rotate && len(item.Scopes) == 1 && item.Scopes[0] == tc.scope
				}
			}
			if !requirement {
				t.Fatal("controlled update does not redeliver the isolated report credential")
			}
		})
	}
}
