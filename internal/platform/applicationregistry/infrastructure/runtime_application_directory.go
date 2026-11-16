package infrastructure

import (
	"context"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// RuntimeApplication 是商业授权运行时可选系统的只读投影。
// 只包含当前租户、当前环境下已登记且部署状态为 READY 的业务应用。
type RuntimeApplication struct {
	Code        string
	Name        string
	Environment string
}

// ListRuntimeApplications 探测当前租户/环境已接入的业务系统。
// 判定链：活跃应用 + 活跃环境 + 活跃登录目标 + 子系统部署状态 READY。
// 不按当前用户权限裁剪：商业授权管理员需要看到租户内全部已部署系统；
// 平台自身（code=platform）不是业务系统，排除。
func (repository *SubsystemOnboardingGORMRepository) ListRuntimeApplications(ctx context.Context, tenantID, environment string) ([]RuntimeApplication, error) {
	if tenantID == "" || environment == "" {
		return nil, application.ErrValidation
	}
	var rows []RuntimeApplication
	err := repository.database.WithContext(ctx).
		Table("subsystem_deployment_state AS deployment").
		Select("DISTINCT app.code AS code, app.name AS name, deployment.environment_code AS environment").
		Joins("JOIN platform_application AS app ON app.tenant_id = deployment.tenant_id AND app.id = deployment.application_id").
		Joins("JOIN platform_application_environment AS env ON env.tenant_id = deployment.tenant_id AND env.id = deployment.environment_id").
		Joins("JOIN platform_application_login_target AS target ON target.tenant_id = deployment.tenant_id AND target.application_id = deployment.application_id AND target.environment_id = deployment.environment_id AND target.status = ?", "ACTIVE").
		Where("deployment.tenant_id = ? AND deployment.environment_code = ? AND deployment.status = ?", tenantID, environment, application.SubsystemDeploymentStatusReady).
		Where("app.status = ? AND env.status = ? AND app.code <> ?", "ACTIVE", "ACTIVE", "platform").
		Order("app.code ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []RuntimeApplication{}
	}
	return rows, nil
}
