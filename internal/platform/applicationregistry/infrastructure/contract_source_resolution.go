package infrastructure

import (
	"os"
	"path/filepath"
	"strings"
)

// 合同来源自动解析（C07 主线 / 组合性"档一：自动缺省+显式覆盖"）。
// 此前 CONTRACT_COUNTERPARTY_SOURCE / CONTRACT_CATEGORY_SOURCE 只能靠运维手改
// env：独立售卖（平台+合同）新装时键缺失，合同按默认 crm/project 启动，在对端
// 缺失时无法独立开张。本文件在受控写入合同运行配置时，按"对端应用接入配置是否
// 就绪"解析出生效来源并连带联动开关；运维显式值（含显式 crm/project）一律保留。
// 解析发生在受控接入/更新的写入时点：后续加购或停用对端时，下一次受控更新自动
// 跟随刷新；既有合同引用的快照不受影响。本地 docker-local 开发流保持既有默认，
// 不走本解析（显式 local 由 local_business_mode 白名单保留）。

// contractSourcePeerRuntimeFiles 映射对端应用到其运行配置路径（部署根相对）。
var contractSourcePeerRuntimeFiles = map[string]string{
	integratedCustomerApplicationCode: "runtime/customer.env",
	integratedProjectApplicationCode:  "runtime/project.env",
}

// contractSourceRule 声明一条来源解析规则：对端就绪 → onValue，未就绪 → offValue；
// 解析生效时仅在开关键同样缺失的情况下连带写一致的联动开关（显式开关保留）。
type contractSourceRule struct {
	sourceKey string
	onValue   string
	offValue  string
	peerApp   string
	flagsOn   map[string]string
	flagsOff  map[string]string
}

var contractSourceRules = []contractSourceRule{
	{
		sourceKey: "CONTRACT_COUNTERPARTY_SOURCE", onValue: "crm", offValue: "local",
		peerApp:  integratedCustomerApplicationCode,
		flagsOn:  map[string]string{"CRM_REFERENCE_ENABLED": "true"},
		flagsOff: map[string]string{"CRM_REFERENCE_ENABLED": "false"},
	},
	{
		sourceKey: "CONTRACT_CATEGORY_SOURCE", onValue: "project", offValue: "local",
		peerApp:  integratedProjectApplicationCode,
		flagsOn:  map[string]string{"PROJECT_INTEGRATION_ENABLED": "true", "PROJECT_CATEGORY_DIRECTORY_ENABLED": "true"},
		flagsOff: map[string]string{"PROJECT_INTEGRATION_ENABLED": "false", "PROJECT_CATEGORY_DIRECTORY_ENABLED": "false"},
	},
}

// contractPeerRuntimeReady 判断对端应用的接入配置是否就绪：运行文件存在、解析
// 成功、目录同步已启用且应用/环境编码可用。目录同步键只在受控接入完成时由平台
// 写入，因此它是"已安装且接入完成"的部署层事实，而不是"进程在跑"的探测。
// 参数：deployRoot 为部署根，app 为对端应用编码。
// 返回值：就绪返回 true；文件缺失/未解析/键不可用均视为未就绪。
func contractPeerRuntimeReady(deployRoot, app string) bool {
	rel, ok := contractSourcePeerRuntimeFiles[app]
	if !ok {
		return false
	}
	path := filepath.Join(deployRoot, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	current := parseEnvironmentValues(string(content))
	return strings.TrimSpace(current["PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED"]) == "true" &&
		usableProductionCredential(current["PLATFORM_APPLICATION_CODE"]) &&
		usableProductionCredential(current["PLATFORM_ENVIRONMENT_CODE"])
}

// keyAbsentInBoth 报告键是否在"现场值"与"本次写入集"中都缺失或为空——缺失才是
// 自动解析的输入；任一侧有值即视为显式声明，保留不覆盖。
// 参数：key 为环境键，current 为现场值，values 为本次写入集。
// 返回值：两侧都缺失返回 true。
func keyAbsentInBoth(key string, current, values map[string]string) bool {
	return strings.TrimSpace(current[key]) == "" && strings.TrimSpace(values[key]) == ""
}

// resolveContractSourceAutoDefaults 解析合同的交易对方/类别来源与联动开关（档一）。
// 规则：
//   - 来源键在场（现场或写入集）→ 视为显式声明，整条规则跳过（连带开关也不动）；
//   - 来源键缺失 → 按 contractPeerRuntimeReady 解析 crm|project / local，并仅当
//     联动开关键同样缺失时写入一致的开关值；
//   - 已知角落：独立交易对方（local）与结算出站集成在合同侧校验互斥；当结算出站
//     已被显式开启而 CRM 未就绪时，本解析不产出任何键（键缺失回落合同既有默认），
//     矛盾交由运维显式裁决，不静默挑选。
//
// 参数：deployRoot 为部署根，current 为现场运行值，values 为本次写入集（就地修改）。
// 返回值：无。
func resolveContractSourceAutoDefaults(deployRoot string, current, values map[string]string) {
	for _, rule := range contractSourceRules {
		if !keyAbsentInBoth(rule.sourceKey, current, values) {
			continue
		}
		if rule.sourceKey == "CONTRACT_COUNTERPARTY_SOURCE" {
			// 结算出站被显式开启时，local 交易对方会在合同启动校验被拒；此时不代
			// 运维挑选来源，保留缺失回落既有默认。
			settlementExplicitlyOn := false
			for _, source := range []map[string]string{current, values} {
				if value, present := explicitRuntimeBool(source["SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED"]); present && value {
					settlementExplicitlyOn = true
					break
				}
			}
			if settlementExplicitlyOn {
				continue
			}
		}
		if contractPeerRuntimeReady(deployRoot, rule.peerApp) {
			values[rule.sourceKey] = rule.onValue
			for key, value := range rule.flagsOn {
				if keyAbsentInBoth(key, current, values) {
					values[key] = value
				}
			}
			continue
		}
		values[rule.sourceKey] = rule.offValue
		for key, value := range rule.flagsOff {
			if keyAbsentInBoth(key, current, values) {
				values[key] = value
			}
		}
	}
}
