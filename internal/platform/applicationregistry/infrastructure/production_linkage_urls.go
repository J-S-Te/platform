package infrastructure

import (
	"os"
	"strconv"
	"strings"
)

// currentRuntimeLinkageModes 读取现场运行值供联动判定。文件缺失或不可读时返回
// 空集，判定退化为"键缺失=联动开启"，与既有升级语义一致。
// 参数：path 为运行环境文件路径。
// 返回值：键值映射；保证非 nil。
func currentRuntimeLinkageModes(path string) map[string]string {
	content, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return parseEnvironmentValues(string(content))
}

// productionLinkageURLGroup 声明一个业务联动的开关判定与其独占的对端业务 URL 键。
// 平台自身端点（platform-api、file-gateway、OIDC）与许可/目录键不属于业务联动，
// 恒注入，不进本表。
type productionLinkageURLGroup struct {
	// offKeys 中任一键显式为 false，或 offSource 命中本地来源模式时联动明确关闭：
	// 本次写入集中移除组内 URL 键。merge 语义保留现场既有值，不主动清场。
	offKeys []string
	// onKeys 中任一键显式为 true 即视为联动明确开启：组内 URL 的最终生效值
	// （本次写入优先、现场值兜底）必须可用，缺失即失败关闭。
	onKeys []string
	// offSourceKey/offSourceValue 声明"本地来源模式"判定（如合同交易对方本地目录）。
	offSourceKey   string
	offSourceValue string
	urlKeys        []string
}

func explicitRuntimeBool(raw string) (value, present bool) {
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, false
	}
	return parsed, true
}

func (group productionLinkageURLGroup) explicitlyOn(modes map[string]string) bool {
	for _, key := range group.onKeys {
		if value, present := explicitRuntimeBool(modes[key]); present && value {
			return true
		}
	}
	return false
}

func (group productionLinkageURLGroup) explicitlyOff(modes map[string]string) bool {
	for _, key := range group.offKeys {
		if value, present := explicitRuntimeBool(modes[key]); present && !value {
			return true
		}
	}
	if group.offSourceKey != "" && strings.TrimSpace(modes[group.offSourceKey]) == group.offSourceValue {
		return true
	}
	return false
}

// productionLinkageURLGroups 从业务能力目录派生某应用的 URL 门禁组（C07）。
// 目录是单一权威：模式键、开关与对端 URL 在 productionBusinessCapabilities 声明，
// 这里只做映射；结算的项目引用键当前无显式布尔开关，不进入目录与门禁。
func productionLinkageURLGroups(code string) []productionLinkageURLGroup {
	groups := make([]productionLinkageURLGroup, 0, 4)
	for _, capability := range productionBusinessCapabilities() {
		if capability.Application != code || len(capability.PeerURLKeys) == 0 {
			continue
		}
		groups = append(groups, productionLinkageURLGroup{
			offKeys:        capability.ToggleKeys,
			onKeys:         capability.ToggleKeys,
			offSourceKey:   capability.LocalSourceKey,
			offSourceValue: capability.LocalSourceValue,
			urlKeys:        capability.PeerURLKeys,
		})
	}
	return groups
}

// applyProductionLinkageURLValues 按联动开关裁剪或校验对端业务 URL（C07）。
// 联动明确关闭：从本次写入集中移除组内 URL 键。
// 联动明确开启：组内 URL 键的最终生效值（本次写入优先、现场值兜底）必须可用
// （非空、非占位），否则失败关闭——不允许"开关开着却指向空端点"。
// 键缺失：保持原样，既有现场升级零破坏。
// 参数：code 为应用编码，current 为现场运行值，values 为本次待写入值（就地修改）。
// 返回值：非法时返回错误。
func applyProductionLinkageURLValues(code string, current, values map[string]string) error {
	for _, group := range productionLinkageURLGroups(code) {
		explicitOff := group.explicitlyOff(current)
		explicitOn := group.explicitlyOn(current)
		if explicitOff {
			for _, key := range group.urlKeys {
				delete(values, key)
			}
			continue
		}
		if !explicitOn {
			continue
		}
		for _, key := range group.urlKeys {
			effective := strings.TrimSpace(values[key])
			if effective == "" {
				effective = strings.TrimSpace(current[key])
			}
			if !usableProductionCredential(effective) {
				return provisioningError("linkage " + key + " is enabled but its peer endpoint is missing or a placeholder")
			}
		}
	}
	return nil
}
