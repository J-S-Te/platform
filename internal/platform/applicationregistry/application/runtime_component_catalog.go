package application

import (
	"context"
	"errors"

	"github.com/J-S-Te/license-core/catalog"
)

// RuntimeComponentSpec 是单个已独立确认的运行时进程在目录中的身份。
// 刻意不包含镜像与覆盖率摘要：普通重新构建不得改变该身份，否则会
// 连带作废已批准的客户端凭据与 ACK 身份。
type RuntimeComponentSpec struct {
	ServiceID            string `json:"service_id"`
	RegistrationProtocol int    `json:"registration_protocol"`
	CatalogVersion       int    `json:"catalog_version"`
	CredentialGeneration string `json:"credential_generation"`
}

// RuntimeComponentSet 由内置目录与所选应用/环境推导生成，
// 绝不接受来自浏览器或运行时 Token 的同名输入，防止伪造组件集合。
type RuntimeComponentSet struct {
	Application        string                 `json:"application"`
	Environment        string                 `json:"environment"`
	CatalogVersion     int                    `json:"catalog_version"`
	Components         []RuntimeComponentSpec `json:"components"`
	RequiredServiceIDs []string               `json:"required_service_ids"`
}

// RuntimeComponentCatalogSource 提供按应用/环境解析运行时组件集合的只读视图；
// 实现必须从服务端内置目录推导，不得回读调用方提交的组件清单。
type RuntimeComponentCatalogSource interface {
	RuntimeComponentSet(context.Context, string, string) (RuntimeComponentSet, error)
}

// ValidateRuntimeComponentSet 校验运行时组件集合与内置目录一致：
// 目录版本必须等于 catalog.Version、组件协议必须为 V2、ServiceID 不得重复、
// 基础设施角色组件一律拒绝；必选组件清单必须恰好等于目录中全部非条件组件。
// 任一不满足返回 ErrValidation，不暴露具体哪个字段失败，避免给伪造者提供探测反馈。
func ValidateRuntimeComponentSet(set RuntimeComponentSet) error {
	if set.Application == "" || set.Environment == "" || set.CatalogVersion != catalog.Version || len(set.Components) == 0 || len(set.Components) > 64 {
		return ErrValidation
	}
	selected := make([]string, 0, len(set.Components))
	seen := map[string]bool{}
	for _, component := range set.Components {
		if seen[component.ServiceID] || component.RegistrationProtocol != catalog.RegistrationProtocolV2 || component.CatalogVersion != set.CatalogVersion || component.CredentialGeneration == "" || !runtimeLicenseID.MatchString(component.CredentialGeneration) {
			return ErrValidation
		}
		catalogComponent, err := catalog.Lookup(set.Application, component.ServiceID)
		if err != nil || catalogComponent.Role == catalog.RoleInfrastructure {
			return ErrValidation
		}
		seen[component.ServiceID] = true
		selected = append(selected, component.ServiceID)
	}
	if err := catalog.ValidateSelection(set.Application, selected); err != nil {
		return ErrValidation
	}
	required := make([]string, 0, len(set.Components))
	for _, component := range set.Components {
		catalogComponent, _ := catalog.Lookup(set.Application, component.ServiceID)
		if !catalogComponent.Conditional {
			required = append(required, component.ServiceID)
		}
	}
	if len(required) != len(set.RequiredServiceIDs) {
		return ErrValidation
	}
	requiredSet := map[string]bool{}
	for _, id := range required {
		requiredSet[id] = true
	}
	for _, id := range set.RequiredServiceIDs {
		if !requiredSet[id] {
			return ErrValidation
		}
	}
	return nil
}

// SelectRuntimeComponentSet 把已通过校验的全量目录集合收窄为受控的部分桥接
// 选择。ids 为空保留全量集合；未知或不重复的 id 一律拒绝（重复 id 返回
// ErrValidation）。RequiredServiceIDs 依据所选成员重新推导，不沿用入参。
func SelectRuntimeComponentSet(set RuntimeComponentSet, ids []string) (RuntimeComponentSet, error) {
	if len(ids) == 0 {
		return set, nil
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if !runtimeLicenseID.MatchString(id) || selected[id] {
			return RuntimeComponentSet{}, ErrValidation
		}
		selected[id] = true
	}
	byID := map[string]RuntimeComponentSpec{}
	for _, component := range set.Components {
		byID[component.ServiceID] = component
	}
	result := RuntimeComponentSet{Application: set.Application, Environment: set.Environment, CatalogVersion: set.CatalogVersion}
	for _, id := range ids {
		component, ok := byID[id]
		if !ok {
			return RuntimeComponentSet{}, ErrValidation
		}
		result.Components = append(result.Components, component)
		catalogComponent, err := catalog.Lookup(set.Application, id)
		if err != nil || catalogComponent.Role == catalog.RoleInfrastructure {
			return RuntimeComponentSet{}, ErrValidation
		}
		if !catalogComponent.Conditional {
			result.RequiredServiceIDs = append(result.RequiredServiceIDs, id)
		}
	}
	if len(result.Components) == 0 {
		return RuntimeComponentSet{}, ErrValidation
	}
	return result, nil
}

// RuntimeLicenseClientIDV2 从实例、应用、环境、服务与凭据代次推导稳定的机器身份。
// 刻意排除镜像、覆盖率与发布摘要，保证普通重新构建后凭据与 ACK 身份不变，
// 避免每次发版都要重新登记全部运行实例。
func RuntimeLicenseClientIDV2(instanceID, app, env, serviceID, credentialGeneration string) string {
	return catalog.ClientIDV2(instanceID, app, env, serviceID, credentialGeneration)
}

// ErrRuntimeComponentCatalogUnavailable 表示内置运行时目录不可用或版本缺失；
// 调用方应停用运行时许可登记入口，而不是放行未校验的组件集合。
var ErrRuntimeComponentCatalogUnavailable = errors.New("runtime component catalog unavailable")
