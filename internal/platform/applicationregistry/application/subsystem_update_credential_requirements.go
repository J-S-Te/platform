package application

// SubsystemUpdateServiceCredentialRequirements 返回受控更新时需要确保的服务凭据需求（逐行自既有实现复制）。
func SubsystemUpdateServiceCredentialRequirements(applicationCode string) []SubsystemUpdateServiceCredentialRequirement {
	fileGateway := SubsystemUpdateServiceCredentialRequirement{
		Purpose: ServiceCredentialFileGatewayWrite, Suffix: "file-gateway-writer",
		ClientName: "文件网关写入器", Scopes: []string{"platform:file:upload"}, Rotate: true,
	}
	switch applicationCode {
	case "contract_management":
		return []SubsystemUpdateServiceCredentialRequirement{
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "合同管理系统 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			// 通知 Secret 只写不可回读。普通更新也重新交付，修复历史运行文件缺键，
			// 不轮换浏览器凭据或给通知客户端附加业务写权限。
			{Purpose: ServiceCredentialNotificationIngest, Suffix: "notification-publisher", ClientName: "合同管理系统 Notification Publisher", Scopes: []string{"notification.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialOwnerDirectoryRead, Suffix: "owner-directory", ClientName: "合同管理系统 Owner Directory Reader", Scopes: []string{"owner_directory.read"}},
			// 外部合同创建会同步校验 CRM 客户和商机。历史环境可能早于该集成能力，
			// 受控更新必须创建或轮换凭据并把明文 Secret 重新下发到运行文件。
			{Purpose: ServiceCredentialCRMContractReferenceRead, Suffix: "crm-contract-reference", ClientName: "合同管理系统 CRM Contract Reference Reader", Scopes: []string{"customer.contract_reference.read"}, Rotate: true},
			{Purpose: ServiceCredentialProjectContractImport, Suffix: "project-integration", ClientName: "合同管理系统 Project Contract Importer", Scopes: []string{"project.contract.import"}, Rotate: true},
			{Purpose: ServiceCredentialSettlementContractIngest, Suffix: "settlement-integration", ClientName: "合同结算事件投递器", Scopes: []string{"settlement.contract.ingest"}},
			fileGateway,
		}
	case "customer_and_opportunity":
		// Audit 与 notification Secret 都是运行时交付凭据。既有安装可能早于
		// notification_ingest 能力，也可能持有交付给其他环境的凭据，
		// 因此每次受控更新都必须轮换并把两个值随环境文件原子重新下发。
		return []SubsystemUpdateServiceCredentialRequirement{
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "客户与商机管理系统 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialNotificationIngest, Suffix: "notification-publisher", ClientName: "客户与商机管理系统 Notification Publisher", Scopes: []string{"notification.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialContractSummaryRead, Suffix: "contract-summary", ClientName: "客户与商机管理系统 Contract Summary Reader", Scopes: []string{"contract.summary.read"}, Rotate: true},
			// CRM 的已签约商机写入接口必须使用独立的 opportunity.signed.write
			// 服务 Client。首次接入按清单创建；受控重试也必须轮换/重建并把明文 Secret
			// 重新交付给 Agent，否则历史部分失败环境会持续缺少 CONTRACT_CLIENT_SECRET。
			{Purpose: ServiceCredentialContractOpportunitySignedWrite, Suffix: "opportunity-intake", ClientName: "客户与商机管理系统 Contract Opportunity Signed Writer", Scopes: []string{"opportunity.signed.write"}, Rotate: true},
			// 客户管理负责人/组织选择依赖平台 Owner Directory；更新流程必须像首次接入一样
			// 确保该机器凭据存在并重新下发，否则运行时会因缺少凭据直接拒绝启动。
			{Purpose: ServiceCredentialOwnerDirectoryRead, Suffix: "owner-directory", ClientName: "客户与商机管理系统 Owner Directory Reader", Scopes: []string{"owner_directory.read"}, Rotate: true},
			{Purpose: ServiceCredentialContractOpportunitySignedCountRead, Suffix: "contract-signed-count", ClientName: "客户与商机管理系统 Contract Opportunity Signed Count Reader", Scopes: []string{"contract.opportunity_signed_count.read"}, Rotate: true},
			fileGateway,
		}
	case "customer_portal":
		// Portal 的服务凭据同时会被写入 portal.env 与 customer.env：门户 API
		// 负责身份映射/邀请校验，CRM 侧的补偿 Worker 负责异步修复跨系统状态。
		// 旧版本部署可能已经创建了这些 OAuth Client，但运行时文件没有收到
		// 明文 Secret；受控更新必须重新签发并原子下发，不能依赖只写密钥回读。
		return []SubsystemUpdateServiceCredentialRequirement{
			// Portal API 在启动与运行期间都会发布审计事件。仅纳入目录的存量环境
			// 也必须像首次接入一样创建并重新交付该凭据，否则生产清单会在任何
			// portal 容器启动前 fail-closed。
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "客户自助门户 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialExternalUserProvision, Suffix: "external-user-provision", ClientName: "客户自助门户 External User Provisioner", Scopes: []string{"external_user.provision"}, Rotate: true},
			{Purpose: ServiceCredentialApplicationRoleAssign, Suffix: "role-assign", ClientName: "客户自助门户 Application Role Assigner", Scopes: []string{"application_role.assign"}, Rotate: true},
			{Purpose: ServiceCredentialApplicationRoleRevoke, Suffix: "role-revoke", ClientName: "客户自助门户 Application Role Revoker", Scopes: []string{"application_role.revoke"}, Rotate: true},
			{Purpose: ServiceCredentialPortalMappingProvision, Suffix: "portal-mapping-provision", ClientName: "客户自助门户 Portal Identity Mapping Provisioner", Scopes: []string{"portal.identity_mapping.provision"}, Rotate: true},
			{Purpose: ServiceCredentialPortalMappingDisable, Suffix: "portal-mapping-disable", ClientName: "客户自助门户 Portal Identity Mapping Disabler", Scopes: []string{"portal.identity_mapping.disable"}, Rotate: true},
			{Purpose: ServiceCredentialPortalInviteVerify, Suffix: "portal-invite-verify", ClientName: "客户自助门户 Portal Invite Verifier", Scopes: []string{"portal.invite.verify"}, Rotate: true},
			{Purpose: ServiceCredentialPortalReportFileRead, Suffix: "portal-report-source", ClientName: "门户报告源只读", Scopes: []string{"platform:file:portal_report"}},
			{Purpose: ServiceCredentialPortalReportDownloadIdentityRead, Suffix: "portal-report-identity", ClientName: "门户下载身份只读", Scopes: []string{"portal.report.download_identity.read"}},
			fileGateway,
		}
	case "data_analysis":
		return []SubsystemUpdateServiceCredentialRequirement{
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "数据看板与统计分析系统 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			// 合同与项目读取凭据必须分离；任一密钥泄露时不能横向读取另一业务域。
			{Purpose: ServiceCredentialContractDashboardRead, Suffix: "contract-dashboard", ClientName: "数据看板合同看板读取器", Scopes: []string{"dashboard.contract.read"}, Rotate: true},
			{Purpose: ServiceCredentialProjectDashboardRead, Suffix: "project-dashboard", ClientName: "数据看板项目看板读取器", Scopes: []string{"dashboard.project.read"}, Rotate: true},
			fileGateway,
		}
	case "project_management":
		// 服务项操作台按人员选择团队负责人/项目经理/工程师，需要只读负责人目录凭据；
		// 人工新建项目必须通过独立机器身份读取已审批合同。受控更新/重试必须像首次
		// 接入一样补齐并重新下发这些凭据，否则运行时会因缺少 Secret 而拒绝部署。
		return []SubsystemUpdateServiceCredentialRequirement{
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "项目管理系统 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialOwnerDirectoryRead, Suffix: "owner-directory", ClientName: "项目管理系统 Owner Directory Reader", Scopes: []string{"owner_directory.read"}, Rotate: true},
			// 自动化规则命中后把站内信投递到平台统一 outbox。
			{Purpose: ServiceCredentialNotificationIngest, Suffix: "notification-ingest", ClientName: "项目管理系统 站内信投递器", Scopes: []string{"notification.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialContractApprovedRead, Suffix: "contract-approved-reader", ClientName: "项目管理系统 Approved Contract Reader", Scopes: []string{"contract.approved.internal.read"}, Rotate: true},
			fileGateway,
		}
	case "settlement", "settlement_and_invoicing":
		// Settlement 的实际应用编码是 settlement。保留旧编码仅用于历史环境
		// 兼容；更新和重试必须补发运行时真实使用的审计、通知、人员目录及
		// 文件网关凭据，不能因为编码漂移静默返回空凭据集合。
		return []SubsystemUpdateServiceCredentialRequirement{
			{Purpose: ServiceCredentialAuditIngest, Suffix: "audit-publisher", ClientName: "结算与开票管理系统 Audit Publisher", Scopes: []string{"audit.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialNotificationIngest, Suffix: "notification-publisher", ClientName: "结算与开票管理系统 Notification Publisher", Scopes: []string{"notification.ingest"}, Rotate: true},
			{Purpose: ServiceCredentialOwnerDirectoryRead, Suffix: "owner-directory", ClientName: "结算与开票管理系统 Owner Directory Reader", Scopes: []string{"owner_directory.read"}, Rotate: true},
			fileGateway,
		}
	default:
		return nil
	}
}
