package infrastructure

import (
	"strconv"
	"strings"
)

// productionBusinessCapability 是业务联动能力的统一声明（C07 主线初建）。
// 此前各子系统的独立/联动模式散落在 8+ 个 env 键里靠运维手改；本表把模式键、
// 显式开关键与对端业务 URL 归一为应用级清单，作为 linkage URL 门禁与能力描述
// （诊断、后续产品选择机制）的单一权威。新增能力只改本表，不改各消费点。
type productionBusinessCapability struct {
	// Application 为应用编码（与 integrated*ApplicationCode 常量一致）。
	Application string
	// Name 为稳定能力名（readyz capabilities 段与诊断输出直接使用）。
	Name string
	// ToggleKeys 中任一键显式 true 即联动明确开启；显式 false 即明确关闭。
	ToggleKeys []string
	// LocalSourceKey/LocalSourceValue 声明"本地来源模式"判定（优先于开关）。
	LocalSourceKey   string
	LocalSourceValue string
	// ModeKey/ModeValues 声明模式型能力（如赢单确认 contract|local_evidence）。
	ModeKey    string
	ModeValues []string
	// PeerURLKeys 为该联动独占的对端业务 URL；平台自身端点不属于任何能力。
	PeerURLKeys []string
}

// explicitlyOn 报告能力是否被显式开启（本地来源模式优先判定为关闭）。
// 参数：capability 为能力声明，modes 为现场运行值。
// 返回值：显式开启返回 true；键缺失一律视为关闭语义下的"未判定"，由消费方
// 决定保守行为（URL 门禁视为开启以保持既有现场零破坏）。
func (capability productionBusinessCapability) explicitlyOn(modes map[string]string) bool {
	if capability.explicitLocalSource(modes) {
		return false
	}
	for _, key := range capability.ToggleKeys {
		if value, err := strconv.ParseBool(strings.TrimSpace(modes[key])); err == nil && value {
			return true
		}
	}
	return false
}

// explicitlyOff 报告能力是否被显式关闭。
// 参数：capability 为能力声明，modes 为现场运行值。
// 返回值：显式 false 或命中本地来源模式时为 true。
func (capability productionBusinessCapability) explicitlyOff(modes map[string]string) bool {
	if capability.explicitLocalSource(modes) {
		return true
	}
	for _, key := range capability.ToggleKeys {
		if value, err := strconv.ParseBool(strings.TrimSpace(modes[key])); err == nil && !value {
			return true
		}
	}
	return false
}

func (capability productionBusinessCapability) explicitLocalSource(modes map[string]string) bool {
	return capability.LocalSourceKey != "" && strings.TrimSpace(modes[capability.LocalSourceKey]) == capability.LocalSourceValue
}

// productionBusinessCapabilities 返回全部已审核业务能力声明。
// 参数：无。
// 返回值：声明切片；新增能力在此追加，消费点（URL 门禁、能力描述）自动跟随。
func productionBusinessCapabilities() []productionBusinessCapability {
	return []productionBusinessCapability{
		{Application: integratedContractApplicationCode, Name: "crm_reference",
			ToggleKeys: []string{"CRM_REFERENCE_ENABLED"}, LocalSourceKey: "CONTRACT_COUNTERPARTY_SOURCE", LocalSourceValue: "local",
			PeerURLKeys: []string{"CRM_REFERENCE_BASE_URL"}},
		{Application: integratedContractApplicationCode, Name: "project_integration",
			ToggleKeys: []string{"PROJECT_INTEGRATION_ENABLED"}, LocalSourceKey: "CONTRACT_CATEGORY_SOURCE", LocalSourceValue: "local",
			PeerURLKeys: []string{"CONTRACT_IMPORT_PROJECT_BASE_URL", "CONTRACT_IMPORT_PROJECT_TOKEN_URL"}},
		{Application: integratedContractApplicationCode, Name: "settlement_outbound",
			ToggleKeys:  []string{"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED"},
			PeerURLKeys: []string{"CONTRACT_IMPORT_FINANCE_BASE_URL", "CONTRACT_IMPORT_FINANCE_TOKEN_URL", "SETTLEMENT_API_BASE_URL", "SETTLEMENT_INTEGRATION_TOKEN_URL"}},
		{Application: integratedContractApplicationCode, Name: "import_history",
			ToggleKeys:  []string{"CONTRACT_IMPORT_HISTORY_ENABLED"},
			PeerURLKeys: []string{"CONTRACT_IMPORT_REPORT_BASE_URL", "CONTRACT_IMPORT_REPORT_TOKEN_URL"}},
		{Application: integratedContractApplicationCode, Name: "counterparty_source",
			ModeKey: "CONTRACT_COUNTERPARTY_SOURCE", ModeValues: []string{"crm", "local"}},
		{Application: integratedContractApplicationCode, Name: "category_source",
			ModeKey: "CONTRACT_CATEGORY_SOURCE", ModeValues: []string{"project", "local"}},

		{Application: integratedCustomerApplicationCode, Name: "win_confirmation",
			ModeKey: "CRM_WIN_CONFIRMATION_MODE", ModeValues: []string{"contract", "local_evidence"}},
		{Application: integratedCustomerApplicationCode, Name: "contract_transfer_subscription",
			ToggleKeys: []string{"CRM_CONTRACT_TRANSFER_SUBSCRIBED"}},

		{Application: integratedProjectApplicationCode, Name: "native_project",
			ToggleKeys: []string{"PROJECT_NATIVE_ENABLED"}},
		{Application: integratedProjectApplicationCode, Name: "contract_integration",
			ToggleKeys: []string{"CONTRACT_INTEGRATION_ENABLED"}},
		{Application: integratedProjectApplicationCode, Name: "approval_validation",
			ToggleKeys:  []string{"CONTRACT_APPROVAL_VALIDATION_ENABLED"},
			PeerURLKeys: []string{"CONTRACT_APPROVAL_VALIDATION_URL"}},

		{Application: integratedSettlementApplicationCode, Name: "settlement_integration",
			ToggleKeys: []string{"SETTLEMENT_INTEGRATION_ENABLED"}},
		{Application: integratedSettlementApplicationCode, Name: "native_financial",
			ToggleKeys: []string{"SETTLEMENT_NATIVE_FINANCIAL_ENABLED"}},
		{Application: integratedSettlementApplicationCode, Name: "crm_credit_sync",
			ToggleKeys: []string{"SETTLEMENT_CRM_CREDIT_SYNC_ENABLED"}},

		{Application: integratedPortalApplicationCode, Name: "customer_directory",
			ModeKey: "PORTAL_CUSTOMER_DIRECTORY_MODE", ModeValues: []string{"crm", "local"}},

		{Application: "data_analysis", Name: "dashboard_embed",
			ToggleKeys: []string{"DASHBOARD_EMBED_ENABLED"}},
		{Application: "data_analysis", Name: "finance_source",
			ModeKey: "DASHBOARD_FINANCE_SOURCE", ModeValues: []string{"integration", "standard"}},
		{Application: "data_analysis", Name: "integration_sync",
			ToggleKeys: []string{"DASHBOARD_INTEGRATION_SYNC_ENABLED"}},
	}
}

// BusinessCapabilityState 是单个能力在给定现场值下的描述。
type BusinessCapabilityState struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Mode    string `json:"mode,omitempty"`
}

// DescribeProductionBusinessCapabilities 按现场运行值描述某应用的能力集合（C14
// 与后续产品选择机制共用的单一出口）。开关型能力：显式关闭 → disabled；其余
// （含键缺失）→ ready，与 URL 门禁的保守口径一致。模式型能力：state 恒为 ready，
// 值未登记时置 mode 为原样值并标记 state=not_configured。
// 参数：code 为应用编码，modes 为现场运行值。
// 返回值：按声明顺序排列的能力状态；未知应用返回空。
func DescribeProductionBusinessCapabilities(code string, modes map[string]string) []BusinessCapabilityState {
	states := make([]BusinessCapabilityState, 0, 8)
	for _, capability := range productionBusinessCapabilities() {
		if capability.Application != code {
			continue
		}
		if capability.ModeKey != "" {
			mode := strings.TrimSpace(modes[capability.ModeKey])
			state := "ready"
			if len(capability.ModeValues) > 0 && mode != "" {
				known := false
				for _, knownMode := range capability.ModeValues {
					if mode == knownMode {
						known = true
						break
					}
				}
				if !known {
					state = "not_configured"
				}
			} else if mode == "" {
				state = "not_configured"
			}
			states = append(states, BusinessCapabilityState{Name: capability.Name, Enabled: mode != "", State: state, Mode: mode})
			continue
		}
		enabled := !capability.explicitlyOff(modes)
		states = append(states, BusinessCapabilityState{Name: capability.Name, Enabled: enabled, State: capabilityStateText(enabled)})
	}
	return states
}

func capabilityStateText(enabled bool) string {
	if enabled {
		return "ready"
	}
	return "disabled"
}
