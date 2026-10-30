package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type productionAccessImageOutput func(context.Context, string, []string, string, ...string) ([]byte, error)

// SubsystemImageReadiness 读取受审拓扑并返回经过本机核验的不可变镜像身份。
// 参数：ctx为读取上下文，app/env为已受控登记的应用与环境。
// 返回值：逐服务镜像事实；业务引用、基础镜像来源或Docker不可核验时失败关闭。
func (p *ProductionComposeSubsystemProvisioner) SubsystemImageReadiness(ctx context.Context, app, env string) ([]application.SubsystemImageReadiness, error) {
	original, err := p.target(app, env)
	if err != nil {
		return nil, err
	}
	// Read-only inventory must not change the target cached by a concurrent deployment.
	target := &productionComposeTarget{config: original.config, runner: original.runner}
	if err = target.validateEnabledServices(ctx); err != nil {
		return nil, err
	}
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	if err = target.runner.Run(ctx, target.config.DeployRoot, os.Environ(), target.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	raw, err := target.runComposeOutput(ctx, "config", "--format", "json")
	if err != nil {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	// Resolved configuration can contain secrets. Keep it in memory, expose images only.
	var config struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil, application.ErrValidation
	}
	services := append([]string{}, target.selectedRuntimeServices()...)
	services = append(services, target.config.Profile.Manifest.Compose.InitializationServices...)
	services = append(services, target.config.Profile.Manifest.Compose.MigrateService, target.config.Profile.Manifest.Compose.CatalogSyncService)
	slices.Sort(services)
	services = slices.Compact(services)
	images := []application.SubsystemImageReadiness{}
	for _, service := range services {
		if service == "" {
			continue
		}
		image := strings.TrimSpace(config.Services[service].Image)
		found, inspectErr := target.accessImageIdentity(ctx, outputRunner.RunOutput, app, service, image)
		if inspectErr != nil {
			return nil, inspectErr
		}
		images = append(images, found)
	}
	if len(images) == 0 {
		return nil, application.ErrValidation
	}
	return images, nil
}

// accessImageIdentity 以Docker实际摘要核验业务镜像及明确受审的基础镜像。
// 参数：ctx为读取上下文，read为只读Docker接口，app/service/image为清单中的精确引用。
// 返回值：存在性与不可变身份；基础镜像摘要缺失、错库或歧义时返回错误。
func (target *productionComposeTarget) accessImageIdentity(ctx context.Context, read productionAccessImageOutput, app, service, image string) (application.SubsystemImageReadiness, error) {
	result := application.SubsystemImageReadiness{ServiceID: service, ImageDigest: image}
	repository := reviewedFoundationRepository(app, service, image)
	if !isImmutableProductionImageReference(image) && repository == "" {
		return result, provisioningError("selected service requires immutable release image: " + service)
	}
	raw, err := read(ctx, target.config.DeployRoot, os.Environ(), target.config.DockerBinary,
		"image", "inspect", "--format", "{{json .RepoDigests}}", image)
	if err != nil {
		return result, nil
	}
	var refs []string
	if json.Unmarshal(raw, &refs) != nil {
		return result, application.ErrValidation
	}
	if repository != "" {
		result.ImageDigest, err = verifiedFoundationDigest(repository, refs)
		result.Present = err == nil
		return result, err
	}
	for _, ref := range refs {
		if strings.HasSuffix(ref, image[strings.LastIndex(image, "@"):]) {
			result.Present = true
			break
		}
	}
	return result, nil
}

// reviewedFoundationRepository 限定受审Compose中明确固定版本的基础服务。
// 参数：app/service/image为应用、基础服务及完整声明引用。
// 返回值：允许核验的官方仓库；其他引用返回空，不接受任意业务tag。
func reviewedFoundationRepository(app, service, image string) string {
	if app != "data_analysis" {
		return ""
	}
	switch {
	case service == "data-analysis-metabase" && image == "metabase/metabase:v0.53.7":
		return "metabase/metabase"
	case service == "data-analysis-metabase-init" && image == "mysql:8.4":
		return "mysql"
	default:
		return ""
	}
}

// verifiedFoundationDigest 将实际本机RepoDigests收敛到同仓库唯一内容身份。
// 参数：repository为受审官方仓库，refs为Docker实际返回的引用。
// 返回值：规范不可变引用；无同源摘要、非法摘要或多内容歧义时失败关闭。
func verifiedFoundationDigest(repository string, refs []string) (string, error) {
	verified := map[string]bool{}
	for _, ref := range refs {
		if !isImmutableProductionImageReference(ref) {
			continue
		}
		name, digest, _ := strings.Cut(ref, "@")
		name = strings.TrimPrefix(name, "docker.io/")
		if repository == "mysql" {
			name = strings.TrimPrefix(name, "library/")
		}
		if name == repository {
			verified[repository+"@"+digest] = true
		}
	}
	if len(verified) != 1 {
		return "", provisioningError("reviewed foundation image has no unique verified repository digest")
	}
	for ref := range verified {
		return ref, nil
	}
	return "", application.ErrValidation
}
