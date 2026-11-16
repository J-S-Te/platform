package bootstrap

import (
	"context"

	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
)

// runtimeApplicationDirectory 把应用注册/部署状态读模型适配到商业授权运行时接口。
// 该适配器不扩大权限边界：仓储只做租户/环境限定的只读查询。
type runtimeApplicationDirectory struct {
	repository *applicationregistryinfrastructure.SubsystemOnboardingGORMRepository
}

func (directory runtimeApplicationDirectory) ListRuntimeApplications(ctx context.Context, tenantID, environment string) ([]licensehttp.RuntimeApplication, error) {
	rows, err := directory.repository.ListRuntimeApplications(ctx, tenantID, environment)
	if err != nil {
		return nil, err
	}
	items := make([]licensehttp.RuntimeApplication, 0, len(rows))
	for _, row := range rows {
		items = append(items, licensehttp.RuntimeApplication{Code: row.Code, Name: row.Name, Environment: row.Environment})
	}
	return items, nil
}
