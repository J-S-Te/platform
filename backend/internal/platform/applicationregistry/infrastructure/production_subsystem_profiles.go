package infrastructure

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"gopkg.in/yaml.v3"
)

const (
	productionSubsystemProfileVersion = 1
	maximumSubsystemProfileBytes      = 256 * 1024
)

// productionSubsystemManifest 是由运维代码审查后随发布包交付的部署白名单。清单只描述
// 固定 Compose 服务、运行时文件及“平台输入 -> 环境变量”映射，不提供命令、脚本或宿主机
// 任意路径字段，因此新增子系统不需要修改 Agent 代码，也不会把浏览器输入变成 shell 参数。
type productionSubsystemManifest struct {
	Version     int                                    `yaml:"version"`
	Default     bool                                   `yaml:"default"`
	Application productionSubsystemApplicationManifest `yaml:"application"`
	Runtime     productionSubsystemRuntimeManifest     `yaml:"runtime"`
	Compose     productionSubsystemComposeManifest     `yaml:"compose"`
}

type productionSubsystemApplicationManifest struct {
	Code        string `yaml:"code"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Environment string `yaml:"environment"`
	PathPrefix  string `yaml:"path_prefix"`
	UpstreamURL string `yaml:"upstream_url"`
	ClientType  string `yaml:"client_type"`
}

type productionSubsystemRuntimeManifest struct {
	RequiredInfrastructureKeys []string                                 `yaml:"required_infrastructure_keys"`
	Files                      []productionSubsystemRuntimeFileManifest `yaml:"files"`
}

type productionSubsystemRuntimeFileManifest struct {
	Path                  string            `yaml:"path"`
	ComposeEnvironmentKey string            `yaml:"compose_environment_key"`
	Bindings              map[string]string `yaml:"bindings"`
	Values                map[string]string `yaml:"values"`
	RequiredExistingKeys  []string          `yaml:"required_existing_keys"`
}

type productionSubsystemComposeManifest struct {
	Profiles           []string                             `yaml:"profiles"`
	DependencyServices []string                             `yaml:"dependency_services"`
	Database           *productionSubsystemDatabaseManifest `yaml:"database"`
	MigrateService     string                               `yaml:"migrate_service"`
	RuntimeServices    []string                             `yaml:"runtime_services"`
	TeardownServices   []string                             `yaml:"teardown_services"`
	ReleaseImageKeys   []string                             `yaml:"release_image_keys"`
}

type productionSubsystemDatabaseManifest struct {
	Service string `yaml:"service"`
	Name    string `yaml:"name"`
}

type productionSubsystemProfile struct {
	SourcePath string
	Manifest   productionSubsystemManifest
}

// LoadProductionSubsystemCapabilities 只返回管理页面可见的无敏感投影。API 与特权 Agent
// 分别读取同一组只读清单；即使前端数据被篡改，Agent 仍会再次按本地清单拒绝未知目标。
func LoadProductionSubsystemCapabilities(deployRoot, profilesDirectory string) (application.SubsystemProvisioningCapabilities, error) {
	profiles, _, err := loadProductionSubsystemProfiles(deployRoot, profilesDirectory)
	if err != nil {
		return application.SubsystemProvisioningCapabilities{}, err
	}
	return productionSubsystemCapabilities(profiles), nil
}

func loadProductionSubsystemProfiles(deployRoot, profilesDirectory string) ([]productionSubsystemProfile, string, error) {
	root, err := canonicalProductionDeployRoot(deployRoot)
	if err != nil {
		return nil, "", err
	}
	profilesDirectory = strings.TrimSpace(profilesDirectory)
	if profilesDirectory == "" {
		profilesDirectory = filepath.Join(root, "subsystems.d")
	} else if !filepath.IsAbs(profilesDirectory) {
		profilesDirectory = filepath.Join(root, profilesDirectory)
	}
	profilesDirectory = filepath.Clean(profilesDirectory)
	resolvedDirectory, err := filepath.EvalSymlinks(profilesDirectory)
	if err != nil || resolvedDirectory != profilesDirectory || !pathWithinRoot(root, resolvedDirectory) {
		return nil, "", errors.New("production subsystem profiles directory is unavailable")
	}
	info, err := os.Stat(resolvedDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, "", errors.New("production subsystem profiles directory permissions are unsafe")
	}

	entries, err := os.ReadDir(resolvedDirectory)
	if err != nil {
		return nil, "", fmt.Errorf("read production subsystem profiles: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		paths = append(paths, filepath.Join(resolvedDirectory, entry.Name()))
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, "", errors.New("production subsystem profiles directory contains no YAML profile")
	}

	profiles := make([]productionSubsystemProfile, 0, len(paths))
	seenTargets := make(map[string]string, len(paths))
	defaultCount := 0
	for _, path := range paths {
		profile, decodeErr := decodeProductionSubsystemProfile(root, path)
		if decodeErr != nil {
			return nil, "", decodeErr
		}
		key := productionSubsystemTargetKey(profile.Manifest.Application.Code, profile.Manifest.Application.Environment)
		if previous, duplicate := seenTargets[key]; duplicate {
			return nil, "", fmt.Errorf("duplicate production subsystem target in %s and %s", previous, path)
		}
		seenTargets[key] = path
		if profile.Manifest.Default {
			defaultCount++
			if defaultCount > 1 {
				return nil, "", errors.New("production subsystem profiles contain more than one default target")
			}
		}
		profiles = append(profiles, profile)
	}
	if defaultCount == 0 {
		profiles[0].Manifest.Default = true
	}
	return profiles, resolvedDirectory, nil
}

func decodeProductionSubsystemProfile(root, path string) (productionSubsystemProfile, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Clean(path) || !pathWithinRoot(root, resolved) {
		return productionSubsystemProfile{}, fmt.Errorf("production subsystem profile %s is unavailable", filepath.Base(path))
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumSubsystemProfileBytes || info.Mode().Perm()&0o022 != 0 {
		return productionSubsystemProfile{}, fmt.Errorf("production subsystem profile %s has unsafe metadata", filepath.Base(path))
	}
	file, err := os.Open(resolved)
	if err != nil {
		return productionSubsystemProfile{}, fmt.Errorf("open production subsystem profile %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(io.LimitReader(file, maximumSubsystemProfileBytes+1))
	decoder.KnownFields(true)
	var manifest productionSubsystemManifest
	if err := decoder.Decode(&manifest); err != nil {
		return productionSubsystemProfile{}, fmt.Errorf("decode production subsystem profile %s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return productionSubsystemProfile{}, fmt.Errorf("production subsystem profile %s contains multiple YAML documents", filepath.Base(path))
		}
		return productionSubsystemProfile{}, fmt.Errorf("decode production subsystem profile %s: %w", filepath.Base(path), err)
	}
	if err := normalizeAndValidateProductionSubsystemManifest(&manifest); err != nil {
		return productionSubsystemProfile{}, fmt.Errorf("invalid production subsystem profile %s: %w", filepath.Base(path), err)
	}
	return productionSubsystemProfile{SourcePath: resolved, Manifest: manifest}, nil
}

func normalizeAndValidateProductionSubsystemManifest(manifest *productionSubsystemManifest) error {
	if manifest == nil || manifest.Version != productionSubsystemProfileVersion {
		return fmt.Errorf("version must be %d", productionSubsystemProfileVersion)
	}
	app := &manifest.Application
	app.Code = strings.ToLower(strings.TrimSpace(app.Code))
	app.Name = strings.TrimSpace(app.Name)
	app.Description = strings.TrimSpace(app.Description)
	app.Environment = strings.ToLower(strings.TrimSpace(app.Environment))
	app.PathPrefix = strings.TrimRight(strings.TrimSpace(app.PathPrefix), "/")
	app.UpstreamURL = strings.TrimRight(strings.TrimSpace(app.UpstreamURL), "/")
	app.ClientType = strings.ToLower(strings.TrimSpace(app.ClientType))
	if !validProductionTargetCode(app.Code, 64) || app.Name == "" || len(app.Name) > 128 || len(app.Description) > 512 ||
		!validProductionTargetCode(app.Environment, 16) || !validProductionPathPrefix(app.PathPrefix) ||
		!validProductionUpstreamURL(app.UpstreamURL) || app.ClientType != "confidential" {
		return errors.New("application target is invalid")
	}

	runtime := &manifest.Runtime
	runtime.RequiredInfrastructureKeys = normalizedProductionEnvironmentKeys(runtime.RequiredInfrastructureKeys)
	if runtime.RequiredInfrastructureKeys == nil || len(runtime.Files) == 0 || len(runtime.Files) > 8 {
		return errors.New("runtime file policy is invalid")
	}
	seenPaths := make(map[string]struct{}, len(runtime.Files))
	seenComposeKeys := make(map[string]struct{}, len(runtime.Files))
	for index := range runtime.Files {
		file := &runtime.Files[index]
		file.Path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file.Path)))
		file.ComposeEnvironmentKey = strings.TrimSpace(file.ComposeEnvironmentKey)
		if file.Path == "." || filepath.IsAbs(file.Path) || file.Path == "runtime" || !strings.HasPrefix(file.Path, "runtime/") ||
			strings.Contains(file.Path, "../") || !validEnvironmentKey(file.ComposeEnvironmentKey) {
			return errors.New("runtime file path or Compose environment key is invalid")
		}
		if _, duplicate := seenPaths[file.Path]; duplicate {
			return errors.New("runtime file path is duplicated")
		}
		if _, duplicate := seenComposeKeys[file.ComposeEnvironmentKey]; duplicate {
			return errors.New("runtime Compose environment key is duplicated")
		}
		seenPaths[file.Path] = struct{}{}
		seenComposeKeys[file.ComposeEnvironmentKey] = struct{}{}
		if len(file.Bindings) == 0 {
			return errors.New("runtime file must contain at least one managed binding")
		}
		for key, source := range file.Bindings {
			source = strings.TrimSpace(source)
			if !validEnvironmentKey(key) || !validProductionBindingSource(source) ||
				!validProductionServiceBindingForApplication(app.Code, source) {
				return errors.New("runtime binding is invalid")
			}
			file.Bindings[key] = source
			if _, duplicate := file.Values[key]; duplicate {
				return errors.New("runtime environment key exists in bindings and values")
			}
		}
		for key, value := range file.Values {
			if !validEnvironmentKey(key) || len(value) > 4096 || !validEnvironmentValue(value) {
				return errors.New("runtime fixed value is invalid")
			}
		}
		file.RequiredExistingKeys = normalizedProductionEnvironmentKeys(file.RequiredExistingKeys)
		if file.RequiredExistingKeys == nil {
			return errors.New("runtime required-existing key is invalid")
		}
	}

	compose := &manifest.Compose
	normalizedProfiles := normalizedProductionServices(compose.Profiles)
	normalizedDependencies := normalizedProductionServices(compose.DependencyServices)
	normalizedRuntimeServices := normalizedProductionServices(compose.RuntimeServices)
	normalizedTeardownServices := normalizedProductionServices(compose.TeardownServices)
	if normalizedProfiles == nil || normalizedDependencies == nil || normalizedRuntimeServices == nil || normalizedTeardownServices == nil {
		return errors.New("Compose profile or service name is invalid")
	}
	compose.Profiles = normalizedProfiles
	compose.DependencyServices = normalizedDependencies
	compose.RuntimeServices = normalizedRuntimeServices
	compose.TeardownServices = normalizedTeardownServices
	if len(compose.RuntimeServices) == 0 || len(compose.ReleaseImageKeys) == 0 ||
		containsReservedProductionService(compose.DependencyServices) || containsReservedProductionService(compose.RuntimeServices) || containsReservedProductionService(compose.TeardownServices) {
		return errors.New("Compose service policy is invalid")
	}
	if len(compose.TeardownServices) == 0 {
		compose.TeardownServices = append([]string(nil), compose.RuntimeServices...)
	}
	compose.MigrateService = strings.TrimSpace(compose.MigrateService)
	if compose.MigrateService != "" && (!validProductionComposeService(compose.MigrateService) || isReservedProductionService(compose.MigrateService)) {
		return errors.New("Compose migrate service is invalid")
	}
	if compose.Database != nil {
		compose.Database.Service = strings.TrimSpace(compose.Database.Service)
		compose.Database.Name = strings.TrimSpace(compose.Database.Name)
		if !validProductionComposeService(compose.Database.Service) || isReservedProductionService(compose.Database.Service) || !validProductionDatabaseName(compose.Database.Name) {
			return errors.New("Compose database policy is invalid")
		}
	}
	seenImages := make(map[string]struct{}, len(compose.ReleaseImageKeys))
	images := make([]string, 0, len(compose.ReleaseImageKeys))
	for _, key := range compose.ReleaseImageKeys {
		key = strings.TrimSpace(key)
		if !validProductionReleaseImageKey(key) || key == "PLATFORM_IMAGE" || key == "FRONTEND_IMAGE" {
			return errors.New("release image key is invalid")
		}
		if _, duplicate := seenImages[key]; duplicate {
			continue
		}
		seenImages[key] = struct{}{}
		images = append(images, key)
	}
	compose.ReleaseImageKeys = images
	return nil
}

func validProductionBindingSource(source string) bool {
	switch source {
	case "issuer", "client_id", "client_secret", "redirect_uri", "logged_out_url", "public_url", "public_url_no_trailing_slash", "public_origin",
		"tenant_id", "application_id", "application_code", "environment", "path_prefix", "upstream_url", "cookie_secure",
		"catalog_publisher_client_id", "catalog_publisher_client_secret", "issuer_security_center_url":
		return true
	}
	parts := strings.Split(source, ".")
	return len(parts) == 3 && parts[0] == "service" && validProductionTargetCode(parts[1], 64) && (parts[2] == "client_id" || parts[2] == "client_secret")
}

// validProductionServiceBindingForApplication 把清单中的用途凭据限制在控制面真正会为
// 该应用创建的最小权限 Client 集合。这样拼写错误或未经实现的新用途会在 API/Agent
// 启动时失败，而不是等控制面已落库、一次性明文已经生成后才在部署阶段失败。
func validProductionServiceBindingForApplication(applicationCode, source string) bool {
	parts := strings.Split(source, ".")
	if len(parts) != 3 || parts[0] != "service" {
		return true
	}
	purpose := parts[1]
	if purpose == application.ServiceCredentialAuditIngest {
		return true
	}
	switch applicationCode {
	case "customer_and_opportunity":
		return purpose == application.ServiceCredentialOwnerDirectoryRead
	case "customer_portal":
		switch purpose {
		case application.ServiceCredentialExternalUserProvision,
			application.ServiceCredentialApplicationRoleAssign,
			application.ServiceCredentialApplicationRoleRevoke,
			application.ServiceCredentialPortalMappingProvision,
			application.ServiceCredentialPortalMappingDisable,
			application.ServiceCredentialPortalInviteVerify:
			return true
		}
	}
	return false
}

func normalizedProductionEnvironmentKeys(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validEnvironmentKey(value) {
			return nil
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func containsReservedProductionService(values []string) bool {
	for _, value := range values {
		if isReservedProductionService(value) {
			return true
		}
	}
	return false
}

func isReservedProductionService(value string) bool {
	switch value {
	case "platform-api", "platform-mysql", "platform-migrate", "frontend", "subsystem-provisioner":
		return true
	default:
		return false
	}
}

func productionSubsystemCapabilities(profiles []productionSubsystemProfile) application.SubsystemProvisioningCapabilities {
	capabilities := application.SubsystemProvisioningCapabilities{Enabled: true, Mode: "production"}
	seenApplications := make(map[string]struct{}, len(profiles))
	seenEnvironments := make(map[string]struct{}, len(profiles))
	defaultIndex := 0
	for index, profile := range profiles {
		app := profile.Manifest.Application
		if profile.Manifest.Default {
			defaultIndex = index
		}
		if _, exists := seenApplications[app.Code]; !exists {
			seenApplications[app.Code] = struct{}{}
			capabilities.SupportedApplicationCodes = append(capabilities.SupportedApplicationCodes, app.Code)
		}
		if _, exists := seenEnvironments[app.Environment]; !exists {
			seenEnvironments[app.Environment] = struct{}{}
			capabilities.SupportedEnvironments = append(capabilities.SupportedEnvironments, app.Environment)
		}
		capabilities.Targets = append(capabilities.Targets, application.SubsystemProvisioningTarget{
			ApplicationCode: app.Code, ApplicationName: app.Name, Description: app.Description,
			Environment: app.Environment, UpstreamURL: app.UpstreamURL, PathPrefix: app.PathPrefix, ClientType: app.ClientType,
		})
	}
	if len(profiles) > 0 {
		app := profiles[defaultIndex].Manifest.Application
		capabilities.DefaultApplicationCode = app.Code
		capabilities.DefaultApplicationName = app.Name
		capabilities.DefaultDescription = app.Description
		capabilities.DefaultEnvironment = app.Environment
		capabilities.DefaultUpstreamURL = app.UpstreamURL
		capabilities.DefaultPathPrefix = app.PathPrefix
		capabilities.DefaultClientType = app.ClientType
	}
	return capabilities
}

func productionSubsystemTargetKey(applicationCode, environment string) string {
	return strings.ToLower(strings.TrimSpace(applicationCode)) + "\x00" + strings.ToLower(strings.TrimSpace(environment))
}

func canonicalProductionDeployRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("production subsystem deploy root is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve production deploy root: %w", err)
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return "", errors.New("production subsystem deploy root must be an existing canonical path")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("production subsystem deploy root permissions are unsafe")
	}
	return resolved, nil
}
