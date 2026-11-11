package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProductionSettlementDashboardConditionalValidation 验证真实运行配置校验器的启停空值契约。
// 参数：t为仅使用生产模板、临时文件及合成身份的测试上下文。
// 返回值：无；合法配置被拒或缺失身份、原因与其他凭据被放行时失败。
func TestProductionSettlementDashboardConditionalValidation(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		overrides    map[string]string
		appCode      string
		wrongBinding bool
		wantError    bool
	}{
		{name: "enabled_reason_empty", enabled: true},
		{name: "disabled_source_missing", enabled: false},
		{name: "enabled_client_missing", enabled: true, overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID": ""}, wantError: true},
		{name: "enabled_oauth_id_missing", enabled: true, overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_OAUTH_CLIENT_ID": ""}, wantError: true},
		{name: "disabled_reason_missing", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_UNAVAILABLE_REASON": ""}, wantError: true},
		{name: "disabled_reason_placeholder", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_UNAVAILABLE_REASON": "PENDING_ONBOARDING"}, wantError: true},
		{name: "disabled_client_placeholder", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID": "REPLACE_WITH_CLIENT"}, wantError: true},
		{name: "disabled_oauth_placeholder", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_OAUTH_CLIENT_ID": "PENDING_ONBOARDING"}, wantError: true},
		{name: "unrelated_secret_missing", enabled: true, overrides: map[string]string{"FILE_GATEWAY_CLIENT_SECRET": ""}, wantError: true},
		{name: "invalid_enabled", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_ENABLED": "not-a-bool"}, wantError: true},
		{name: "invalid_numeric_enabled", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_ENABLED": "1"}, wantError: true},
		{name: "enabled_flag_missing", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_ENABLED": ""}, wantError: true},
		{name: "invalid_reason", overrides: map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_UNAVAILABLE_REASON": "invalid\x00reason"}, wantError: true},
		{name: "foreign_application", enabled: true, appCode: "data_analysis", wantError: true},
		{name: "wrong_binding", enabled: true, wrongBinding: true, wantError: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			target, input := settlementDashboardProductionFixture(t, "settlement")
			input.CatalogPublisherClientID = "synthetic-catalog-publisher"
			input.CatalogPublisherClientSecret = "synthetic-catalog-secret"
			if item.enabled {
				related := dashboardReceiverFixture().RelatedServiceClients[0]
				related.Client.TenantID = input.TenantID
				input.RelatedServiceClients = append(input.RelatedServiceClients, related)
			}
			if err := target.writeRuntimeConfiguration(input); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			values := parseEnvironmentValues(string(content))
			for key, value := range item.overrides {
				values[key] = value
			}
			content = nil
			for key, value := range values {
				content = append(content, []byte(key+"="+value+"\n")...)
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			if item.appCode != "" {
				target.config.Profile.Manifest.Application.Code = item.appCode
			}
			if item.wrongBinding {
				target.config.Profile.Manifest.Runtime.Files[0].Bindings["SETTLEMENT_DASHBOARD_MACHINE_UNAVAILABLE_REASON"] = "service.audit_ingest.client_secret"
			}
			err = target.validateRuntimeIntegrationConfiguration()
			if (err != nil) != item.wantError {
				t.Fatalf("validation error=%v, wantError=%v", err, item.wantError)
			}
		})
	}
}
