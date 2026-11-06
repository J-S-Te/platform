package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/J-S-Te/license-core/catalog"
)

// productionOptionalFeature 只描述审核清单中可省略的基础设施，不改变业务许可消费者集合。
type productionOptionalFeature struct {
	Name                   string   `yaml:"name"`
	RuntimeFile            string   `yaml:"runtime_file"`
	EnabledKey             string   `yaml:"enabled_key"`
	DefaultEnabled         bool     `yaml:"default_enabled"`
	RequiredExistingKeys   []string `yaml:"required_existing_keys"`
	RuntimeServices        []string `yaml:"runtime_services"`
	InitializationServices []string `yaml:"initialization_services"`
}

// validateOptionalFeaturePolicies 核对可选基础设施的归属和配置边界。
// 参数：manifest为已规范化的受控应用清单。
// 返回值：无错误表示不能借功能开关省略业务API、Worker或安全配置。
func validateOptionalFeaturePolicies(manifest *productionSubsystemManifest) error {
	if len(manifest.OptionalFeatures) > 16 {
		return errors.New("too many optional features")
	}
	seen := map[string]bool{}
	ownedServices := map[string]bool{}
	for _, feature := range manifest.OptionalFeatures {
		if !validProductionTargetCode(feature.Name, 64) || seen[feature.Name] {
			return errors.New("optional feature name is invalid or duplicated")
		}
		seen[feature.Name] = true
		// 现行目录只审核这一项选购基础设施；后续能力须明确扩展契约。
		if manifest.Application.Code != "data_analysis" || feature.Name != "dashboard_embed" ||
			feature.EnabledKey != "DASHBOARD_EMBED_ENABLED" || len(feature.RuntimeServices) == 0 {
			return errors.New("optional feature is outside reviewed capability policy")
		}
		if err := validateOptionalFeatureMembers(manifest, feature, ownedServices); err != nil {
			return err
		}
	}
	return nil
}

// validateOptionalFeatureMembers 验证功能只使用自身已声明的文件、配置和基础设施。
// 参数：manifest为应用清单，feature为功能策略，seen记录已被其他功能引用的服务。
// 返回值：未声明、重复或业务消费者被选择时返回策略错误。
func validateOptionalFeatureMembers(manifest *productionSubsystemManifest, feature productionOptionalFeature, seen map[string]bool) error {
	found := false
	for _, file := range manifest.Runtime.Files {
		if file.Path == feature.RuntimeFile {
			found = true
		}
	}
	if !found || normalizedProductionEnvironmentKeys(feature.RequiredExistingKeys) == nil {
		return errors.New("optional feature runtime policy is invalid")
	}
	for _, key := range feature.RequiredExistingKeys {
		if key != "METABASE_EMBEDDING_SECRET" {
			return errors.New("optional feature cannot relax unrelated configuration")
		}
	}
	for _, service := range feature.RuntimeServices {
		component, err := catalog.Lookup(manifest.Application.Code, service)
		if err != nil || component.Role != catalog.RoleInfrastructure || seen[service] ||
			!slices.Contains(manifest.Compose.RuntimeServices, service) {
			return errors.New("optional runtime service must be a unique application infrastructure")
		}
		seen[service] = true
	}
	for _, service := range feature.InitializationServices {
		if !slices.Contains(manifest.Compose.InitializationServices, service) || seen[service] {
			return errors.New("optional initialization service must be uniquely declared")
		}
		seen[service] = true
	}
	return nil
}

// optionalFeatureEnabled 读取已审核功能的显式运行模式，缺文件时使用兼容默认。
// 参数：feature为审核策略。
// 返回值：模式值；不安全文件、读取失败或非法布尔值返回错误，不静默关闭功能。
func (target *productionComposeTarget) optionalFeatureEnabled(feature productionOptionalFeature) (bool, error) {
	path := filepath.Join(target.config.DeployRoot, feature.RuntimeFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return feature.DefaultEnabled, nil
	}
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if err != nil || resolveErr != nil || resolved != path || !info.Mode().IsRegular() ||
		!pathWithinRoot(target.config.DeployRoot, resolved) || info.Mode().Perm()&0077 != 0 {
		return false, provisioningError("optional feature runtime file is unsafe")
	}
	values, err := readEnvironmentValues(path)
	if err != nil {
		return false, provisioningError("optional feature runtime cannot be read")
	}
	raw := strings.TrimSpace(values[feature.EnabledKey])
	if raw == "" {
		return feature.DefaultEnabled, nil
	}
	if raw != "true" && raw != "false" {
		return false, provisioningError("optional feature flag must be true or false")
	}
	return raw == "true", nil
}

// validateOptionalFeatureRuntime 在运行操作前检查模式及启用功能的必要材料。
// 参数：无。
// 返回值：启用但缺材料或开关非法时返回脱敏错误。
func (target *productionComposeTarget) validateOptionalFeatureRuntime() error {
	for _, feature := range target.config.Profile.Manifest.OptionalFeatures {
		enabled, err := target.optionalFeatureEnabled(feature)
		if err != nil {
			return err
		}
		if enabled {
			path := filepath.Join(target.config.DeployRoot, feature.RuntimeFile)
			if err := validateProductionRequiredEnvironmentKeys(path, feature.RequiredExistingKeys,
				"enabled optional feature configuration is incomplete"); err != nil {
				return err
			}
		}
	}
	return nil
}

// selectOptionalServices 从原服务集合去掉显式关闭的基础设施或其初始化任务。
// 参数：services为原有集合，initialization区分初始化与长期运行进程。
// 返回值：保留顺序的集合；非法模式保持原集合，由操作前校验拒绝执行。
func (target *productionComposeTarget) selectOptionalServices(services []string, initialization bool) []string {
	disabled := map[string]bool{}
	for _, feature := range target.config.Profile.Manifest.OptionalFeatures {
		enabled, err := target.optionalFeatureEnabled(feature)
		if err != nil || enabled {
			continue
		}
		members := feature.RuntimeServices
		if initialization {
			members = feature.InitializationServices
		}
		for _, service := range members {
			disabled[service] = true
		}
	}
	selected := make([]string, 0, len(services))
	for _, service := range services {
		if !disabled[service] {
			selected = append(selected, service)
		}
	}
	return selected
}

// optionalFeatureValues 保留已配置模式并显式交付默认，避免重试切换产品能力。
// 参数：path为正在发布的运行文件，values接收待交付字段。
// 返回值：模式无法核实时返回错误，不发布部分配置。
func (target *productionComposeTarget) optionalFeatureValues(path string, values map[string]string) error {
	for _, feature := range target.config.Profile.Manifest.OptionalFeatures {
		if filepath.Join(target.config.DeployRoot, feature.RuntimeFile) != path {
			continue
		}
		enabled, err := target.optionalFeatureEnabled(feature)
		if err != nil {
			return err
		}
		values[feature.EnabledKey] = booleanEnvironmentValue(enabled)
	}
	return nil
}
