package infrastructure

import (
	"context"
	"errors"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// runtimeRegistrationMatches 用途：核对选中消费者是否已按当前目录登记，避免镜像相同就误判无需操作。
// 参数：ctx 为上下文，tenant/code/environment 为受控目标。
// 返回值：返回登记是否完整；库存或数据库不可读时返回错误，不默认完整。
func (tracker *SubsystemAccessConfigurationTracker) runtimeRegistrationMatches(ctx context.Context, tenant, code, environment string) (bool, error) {
	source := tracker.builtin
	if source == nil {
		source = BuiltinRuntimeComponentCatalog{}
	}
	set, err := source.RuntimeComponentSet(ctx, code, environment)
	if err != nil {
		return false, err
	}
	images, reported, err := tracker.registrationImages.ReadSubsystemAccessImages(ctx, code, environment)
	if err != nil {
		return false, err
	}
	if !reported {
		return false, errors.New("runtime enrollment image inventory unavailable")
	}
	allowed := map[string]bool{}
	for _, component := range set.Components {
		allowed[component.ServiceID] = true
	}
	selected := []string{}
	for _, image := range images {
		if allowed[image.ServiceID] {
			selected = append(selected, image.ServiceID)
		}
	}
	expected, err := application.SelectRuntimeComponentSet(set, selected)
	if err != nil || len(selected) == 0 {
		return false, application.ErrValidation
	}
	var members []struct {
		ServiceID            string
		RegistrationProtocol int
		CatalogVersion       int
	}
	err = tracker.database.WithContext(ctx).Table("license_runtime_member AS member").
		Select("member.service_id, member.registration_protocol, member.catalog_version").
		Joins("JOIN platform_oauth_client AS client ON client.client_id = member.oauth_client_id AND client.tenant_id = ?", tenant).
		Joins("JOIN platform_application AS app ON app.id = client.application_id AND app.tenant_id = client.tenant_id AND app.code = ?", code).
		Joins("JOIN platform_application_environment AS env ON env.id = client.environment_id AND env.application_id = app.id AND env.tenant_id = app.tenant_id AND env.environment = ?", environment).
		Where("member.application = ? AND member.environment IN ? AND member.retired_at IS NULL AND client.status = ? AND app.status = ? AND env.status = ?", code, subsystemAccessEnvironmentAliases(environment), "ACTIVE", "ACTIVE", "ACTIVE").Scan(&members).Error
	if err != nil {
		return false, err
	}
	if len(members) != len(expected.Components) {
		return false, nil
	}
	byID := map[string]bool{}
	for _, member := range members {
		if member.RegistrationProtocol != 2 || member.CatalogVersion != set.CatalogVersion || byID[member.ServiceID] {
			return false, nil
		}
		byID[member.ServiceID] = true
	}
	for _, component := range expected.Components {
		if !byID[component.ServiceID] {
			return false, nil
		}
	}
	return true, nil
}
