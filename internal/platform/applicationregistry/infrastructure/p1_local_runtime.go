package infrastructure

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func p1CredentialValues(input application.SubsystemProvisioningInput, purpose, idKey, secretKey string, current, values map[string]string) error {
	credential, ok := input.RegisteredServiceCredential(purpose)
	if !ok {
		return provisioningError("new business service credential is unavailable")
	}
	client := credential.OAuthClient
	if err := validateP1ServiceCredential(input, purpose, client); err != nil {
		return err
	}
	if credential.PlaintextSecret != "" {
		values[secretKey] = credential.PlaintextSecret
	} else if !usableProductionCredential(current[secretKey]) || current[idKey] != client.ClientID {
		return provisioningError("new service credential delivery requires controlled RETRY")
	}
	values[idKey] = client.ClientID
	return nil
}

// p1LocalRuntimeValues 填充本地受控集成配置并拒绝不完整的机器身份。
// 参数：input 为登记输入，path 为运行文件，values 接收值，fresh 标识首次接入。
// 返回值：无错误表示配置完整；否则返回配置或身份错误。
func (provisioner *LocalDockerSubsystemProvisioner) p1LocalRuntimeValues(input application.SubsystemProvisioningInput, path string, values map[string]string, fresh bool) error {
	if input.ApplicationCode == "reimbursement" {
		return reimbursementLocalRuntimeValues(input, path, values, fresh)
	}
	if input.ApplicationCode != "contract_management" && input.ApplicationCode != "customer_portal" &&
		input.ApplicationCode != "settlement" && input.ApplicationCode != "project_management" &&
		input.ApplicationCode != "data_analysis" {
		return nil
	}
	current := map[string]string{}
	if _, err := os.Stat(path); err == nil {
		var err error
		if input.ApplicationCode == "settlement" {
			current, err = readSettlementSourceValues(path)
		} else {
			current, err = readEnvironmentValues(path)
		}
		if err != nil {
			return provisioningError("read business runtime configuration")
		}
	} else if !os.IsNotExist(err) {
		return provisioningError("read business runtime configuration")
	}
	if input.ApplicationCode == "data_analysis" {
		return dataAnalysisDashboardRuntimeValues(input, current, values)
	}
	if input.ApplicationCode == "project_management" {
		if err := projectSettlementReferenceRuntimeValues(input, values); err != nil {
			return err
		}
		return projectReimbursementRuntimeValues(input, values)
	}
	if input.ApplicationCode == "contract_management" {
		if err := p1CredentialValues(input, application.ServiceCredentialSettlementContractIngest, "SETTLEMENT_INTEGRATION_CLIENT_ID", "SETTLEMENT_INTEGRATION_CLIENT_SECRET", current, values); err != nil {
			return err
		}
		values["SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED"] = "true"
		values["SETTLEMENT_API_BASE_URL"] = "http://settlement-api:8085"
		values["SETTLEMENT_INTEGRATION_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["SETTLEMENT_INTEGRATION_AUDIENCE"] = "basic-platform-application"
		return nil
	}
	if input.ApplicationCode == "settlement" {
		if err := settlementDashboardRuntimeValues(input, values); err != nil {
			return err
		}
		if err := settlementRoleRuntimeValues(input, current, values); err != nil {
			return err
		}
		// 本地 dev 的结算子系统以 SETTLEMENT_DEVELOPMENT_AUTH=true 运行，且其 .env.local 不含浏览器
		// 登录 OIDC 配置（settlement-dev-web 客户端要等接入的「同步 Client」步骤才创建）。结算自身的
		// 配置校验明确拒绝「开发鉴权 + 平台机器令牌/合同集成」组合（Settlement/internal/config/config.go:159-164）。
		// 在该环境下下发机器令牌会让结算 API 启动即配置失败，接入永远无法完成，因此保持原样。
		if strings.EqualFold(strings.TrimSpace(current["SETTLEMENT_DEVELOPMENT_AUTH"]), "true") {
			return nil
		}
		if err := settlementContractRuntimeValues(input, current, values); err != nil {
			return err
		}
		values["CONTRACT_INTEGRATION_AUDIENCE"] = "basic-platform-application"
		values["PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["PLATFORM_MACHINE_TOKEN_ISSUER"] = "basic-platform"
		values["OIDC_TENANT_ID"] = input.TenantID
		values["PLATFORM_ENVIRONMENT_CODE"] = input.Environment
		return nil
	}
	localPortal, modeErr := localPortalMode(current, values)
	if modeErr != nil {
		return modeErr
	}
	if localPortal {
		if _, err := os.Stat(path); err == nil {
			if _, err := localPortalModeFromFile(path); err != nil {
				return err
			}
		}
	} else {
		if err := p1CredentialValues(input, application.ServiceCredentialPortalReportDownloadIdentityRead, "PORTAL_REPORT_IDENTITY_CLIENT_ID", "PORTAL_REPORT_IDENTITY_CLIENT_SECRET", current, values); err != nil {
			return err
		}
		values["PORTAL_REPORT_IDENTITY_URL"] = "http://customer-api:8090/customer-opportunity/api/v1/internal/portal-report/download-identity"
	}
	values["PORTAL_REPORT_STORE_ROOT"] = "/app/data/portal-reports"
	values["PORTAL_PDF_CONFIG_ROOT"] = "/app/data/portal-reports/pdfcpu"
	values["PORTAL_CJK_FONT_PATH"] = "/app/assets/fonts/UIPPortalSans-Regular.ttf"
	if !fresh {
		secrets, err := portalRuntimeSecrets(path)
		if err != nil {
			return provisioningError("prepare Portal report keys")
		}
		for key, value := range secrets {
			values[key] = value
		}
	}
	if !validProductionGeneratedEnvironmentValue("PORTAL_REPORT_CONTENT_KEY_BASE64", values["PORTAL_REPORT_CONTENT_KEY_BASE64"]) || !validProductionGeneratedEnvironmentValue("PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", values["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"]) || values["PORTAL_REPORT_CONTENT_KEY_BASE64"] == values["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"] {
		return provisioningError("report vault requires distinct valid content and descriptor keys")
	}
	if localPortal {
		if err := portalLocalRuntimeValues(input, current, values); err != nil {
			return err
		}
	}
	workerPath := strings.TrimSuffix(path, ".env.portal.local") + ".env.portal-report-worker.local"
	if workerPath == path+".env.portal-report-worker.local" {
		workerPath = filepath.Join(filepath.Dir(path), ".env.portal-report-worker.local")
	}
	worker := map[string]string{}
	if _, err := os.Stat(workerPath); err == nil {
		var err error
		worker, err = readEnvironmentValues(workerPath)
		if err != nil {
			return provisioningError("read report worker runtime")
		}
	} else if !os.IsNotExist(err) {
		return provisioningError("read report worker runtime")
	}
	workerValues := map[string]string{"PORTAL_REPORT_WORKER_MYSQL_DSN": values["PORTAL_MYSQL_DSN"], "PORTAL_OIDC_TENANT_ID": input.TenantID, "PORTAL_REPORT_STORE_ROOT": values["PORTAL_REPORT_STORE_ROOT"], "PORTAL_REPORT_CONTENT_KEY_BASE64": values["PORTAL_REPORT_CONTENT_KEY_BASE64"], "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64": values["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"], "PORTAL_REPORT_SOURCE_BASE_URL": "http://file-gateway:8086", "PORTAL_REPORT_SOURCE_TOKEN_URL": "http://platform-api:8080/oauth2/token", "PLATFORM_APPLICATION_CODE": "customer_portal", "PLATFORM_ENVIRONMENT_CODE": input.Environment}
	if localPortal {
		workerValues["PORTAL_CUSTOMER_DIRECTORY_MODE"] = "local"
		workerValues["PORTAL_REPORT_WORKER_INGEST_ONLY"] = "true"
	} else {
		workerValues["PORTAL_CUSTOMER_DIRECTORY_MODE"] = "crm"
	}
	if !usableProductionCredential(worker["PORTAL_REPORT_WORKER_INGEST_ONLY"]) {
		workerValues["PORTAL_REPORT_WORKER_INGEST_ONLY"] = "true"
	}
	if localPortal {
		if err := portalLocalCredentialValues(input, application.ServiceCredentialPortalReportFileRead, "PORTAL_REPORT_SOURCE", worker, workerValues); err != nil {
			return err
		}
	} else {
		if err := p1CredentialValues(input, application.ServiceCredentialPortalReportFileRead, "PORTAL_REPORT_SOURCE_CLIENT_ID", "PORTAL_REPORT_SOURCE_CLIENT_SECRET", worker, workerValues); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(workerPath); os.IsNotExist(err) {
		if err := os.WriteFile(workerPath, []byte("# Controlled independent report worker runtime\n"), 0600); err != nil {
			return provisioningError("prepare report worker runtime")
		}
	} else if err != nil {
		return provisioningError("prepare report worker runtime")
	}
	return updateProductionRuntimeGroup([]runtimeEnvironmentUpdate{{path: workerPath, values: workerValues}})
}

// p1ServiceCredentialScopes 是严格机器身份每个用途唯一允许的 scope。
// 必须覆盖 p1StrictServicePurposes 的全部用途；缺失时校验会因空 scope 判为不匹配。
var p1ServiceCredentialScopes = map[string]string{
	application.ServiceCredentialReimbursementProjectReferenceRead:  "reimbursement.project_reference.read",
	application.ServiceCredentialProjectReimbursementCostWrite:      "project.reimbursement_cost.write",
	application.ServiceCredentialSettlementDashboardRead:            "dashboard.settlement.read",
	application.ServiceCredentialContractImportReportRead:           "contract.import.report.read",
	application.ServiceCredentialAnalysisContractImportReportStatus: "analysis.contract-import.report-status",
	application.ServiceCredentialSettlementProjectReferenceRead:     "settlement.project_reference.read",
	application.ServiceCredentialOwnerDirectoryRead:                 "owner_directory.read",
	application.ServiceCredentialNotificationIngest:                 "notification.ingest",
	application.ServiceCredentialFileGatewayWrite:                   "platform:file:upload",
	application.ServiceCredentialAuditIngest:                        "audit.ingest",
	application.ServiceCredentialSettlementContractIngest:           "settlement.contract.ingest",
	application.ServiceCredentialCRMContractLinkWrite:               "opportunity.signed.write",
	application.ServiceCredentialPortalReportFileRead:               "platform:file:portal_report",
	application.ServiceCredentialPortalReportDownloadIdentityRead:   "portal.report.download_identity.read",
}

// validateP1ServiceCredential 检查机器身份归属及用途的单scope约束。
// 参数：input 为目标登记环境，purpose 为用途，client 为持久身份元数据。
// 返回值：匹配时为空，否则为拒绝原因。
func validateP1ServiceCredential(input application.SubsystemProvisioningInput, purpose string, client application.OAuthClientView) error {
	if purpose == application.ServiceCredentialSettlementDashboardRead &&
		(input.ApplicationCode != "data_analysis" ||
			client.ClientID != "data_analysis-"+input.Environment+"-settlement-dashboard") {
		return provisioningError("settlement dashboard credential caller mismatch")
	}
	if input.TenantID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || client.ID == "" {
		return provisioningError("new business credential registered identity is unavailable")
	}
	if client.TenantID != input.TenantID || client.ApplicationID != input.ApplicationID || client.EnvironmentID != input.EnvironmentID || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || client.Status != "ACTIVE" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" {
		return provisioningError("new business service credential target mismatch")
	}
	expectedScope := p1ServiceCredentialScopes[purpose]
	scopeValid := len(client.Scopes) == 1 && client.Scopes[0] == expectedScope
	if purpose == application.ServiceCredentialFileGatewayWrite {
		scopeValid = len(client.Scopes) == 3
		for _, required := range []string{"platform:file:upload", "platform:file:bind", "platform:file:download"} {
			count := 0
			for _, scope := range client.Scopes {
				if scope == required {
					count++
				}
			}
			scopeValid = scopeValid && count == 1
		}
	}
	if !scopeValid {
		return provisioningError("new business service credential scope mismatch")
	}
	if client.ClientID == "" {
		return provisioningError("new business service client identity is unavailable")
	}
	return nil
}
