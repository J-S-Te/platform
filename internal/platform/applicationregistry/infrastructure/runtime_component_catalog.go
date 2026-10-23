package infrastructure

import (
	"context"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/license-core/catalog"
)

// BuiltinRuntimeComponentCatalog 编译进平台/Agent。它不读取运行时批准文件，
// 也绝不接受调用方提供的列表。
type BuiltinRuntimeComponentCatalog struct{}

// BuiltinComponents 把有限目录暴露给协调层，不依赖任何批准文件或调用方提供的组件列表。
func (BuiltinRuntimeComponentCatalog) BuiltinComponents(ctx context.Context, app, env string) ([]catalog.Component, error) {
	if ctx == nil || app == "" || env == "" {
		return nil, application.ErrValidation
	}
	components, err := catalog.Components(app)
	if err != nil {
		return nil, application.ErrRuntimeComponentCatalogUnavailable
	}
	return components, nil
}

// RuntimeComponentSet 由内置目录推导运行时组件集合：基础设施角色被排除在凭证发放之外，
// 非 conditional 组件进入 RequiredServiceIDs，凭据代际绑定 catalog.Version，从而保证
// 目录升级会自然触发凭据轮换。
func (BuiltinRuntimeComponentCatalog) RuntimeComponentSet(ctx context.Context, app, env string) (application.RuntimeComponentSet, error) {
	if ctx == nil || app == "" || env == "" {
		return application.RuntimeComponentSet{}, application.ErrValidation
	}
	components, err := catalog.Components(app)
	if err != nil {
		return application.RuntimeComponentSet{}, application.ErrRuntimeComponentCatalogUnavailable
	}
	generation := fmt.Sprintf("catalog-%d", catalog.Version)
	set := application.RuntimeComponentSet{Application: app, Environment: env, CatalogVersion: catalog.Version}
	for _, component := range components {
		if component.Role == catalog.RoleInfrastructure {
			continue
		}
		set.Components = append(set.Components, application.RuntimeComponentSpec{
			ServiceID: component.ServiceID, RegistrationProtocol: catalog.RegistrationProtocolV2,
			CatalogVersion: catalog.Version, CredentialGeneration: generation,
		})
		if !component.Conditional {
			set.RequiredServiceIDs = append(set.RequiredServiceIDs, component.ServiceID)
		}
	}
	if err := application.ValidateRuntimeComponentSet(set); err != nil {
		return application.RuntimeComponentSet{}, err
	}
	return set, nil
}
