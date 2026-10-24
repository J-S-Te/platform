package bootstrap

import (
	"context"
	"strings"

	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	coordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	licenseruntime "github.com/J-S-Te/license-core/runtime"
	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// handoverRegistryReader 只暴露判定适用性所需的注册查询。
type handoverRegistryReader interface {
	ListApplications(context.Context, string, applicationregistryapplication.PageRequest) (applicationregistryapplication.PageResult[applicationregistryapplication.Application], error)
	ListEnvironments(context.Context, string, string, applicationregistryapplication.PageRequest) (applicationregistryapplication.PageResult[applicationregistryapplication.Environment], error)
}

// handoverLicenseStatusReader 读取某应用的商业授权运行状态。
type handoverLicenseStatusReader interface {
	Status(context.Context, string) (coordination.Status, error)
}

// personnelHandoverInventory 由控制面事实推导每个系统能否参与交接：
// 未登记或未登记目标环境 = 不适用；已登记但授权未生效/快照过期 = 授权拒绝；
// 授权状态不可读 = 不可达。三者都属于"必须阻断"或"不适用"，绝不静默跳过。
type personnelHandoverInventory struct {
	registry    handoverRegistryReader
	licenses    handoverLicenseStatusReader
	systems     []string
	environment string
}

const handoverInventoryPageSize = 200

// Systems 返回每个交接系统的适用性判定（已登记、授权状态、目标环境），
// 用于人事交接前阻断不适用的系统。
func (inventory *personnelHandoverInventory) Systems(ctx context.Context, tenantID string) ([]identityapplication.HandoverSystemState, error) {
	applications, err := inventory.registry.ListApplications(ctx, tenantID, applicationregistryapplication.PageRequest{Page: 1, PageSize: handoverInventoryPageSize})
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]applicationregistryapplication.Application, len(applications.Items))
	for _, application := range applications.Items {
		byCode[strings.TrimSpace(application.Code)] = application
	}
	states := make([]identityapplication.HandoverSystemState, 0, len(inventory.systems))
	for _, system := range inventory.systems {
		application, registered := byCode[system]
		if !registered {
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemNotInstalled})
			continue
		}
		environments, err := inventory.registry.ListEnvironments(ctx, tenantID, application.ID, applicationregistryapplication.PageRequest{Page: 1, PageSize: handoverInventoryPageSize})
		if err != nil {
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemUnreachable})
			continue
		}
		if !hasHandoverEnvironment(environments.Items, inventory.environment) {
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemNotInstalled})
			continue
		}
		if inventory.licenses == nil {
			// 未接线授权状态读取时不假装已确认；交接派发仍受运行批准与许可证闸门约束。
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemApplicable})
			continue
		}
		status, err := inventory.licenses.Status(ctx, system)
		if err != nil {
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemUnreachable})
			continue
		}
		if status.State != licenseruntime.Enforced || !status.SnapshotCurrent {
			states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemLicenseDenied})
			continue
		}
		states = append(states, identityapplication.HandoverSystemState{System: system, Capability: identityapplication.HandoverSystemApplicable})
	}
	return states, nil
}

// hasHandoverEnvironment 判断应用是否登记了目标环境；未指定环境时只要存在登记即视为已安装。
func hasHandoverEnvironment(environments []applicationregistryapplication.Environment, expected string) bool {
	if len(environments) == 0 {
		return false
	}
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return true
	}
	for _, environment := range environments {
		if strings.EqualFold(strings.TrimSpace(environment.Environment), expected) && !strings.EqualFold(strings.TrimSpace(environment.Status), "DISABLED") {
			return true
		}
	}
	return false
}
