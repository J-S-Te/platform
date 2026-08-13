package infrastructure

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
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
	// InitialAdminRoles 是接入时给操作人授予的初始管理员角色（按该子系统目录中的角色码）。
	// 指针区分“未声明（回退平台默认）”与“显式空（不授予内部管理员）”。
	InitialAdminRoles *[]string `yaml:"initial_admin_roles"`
	// AllowedServiceBindings 是该应用可绑定的服务用途客户端白名单（audit_ingest 恒允许）。
	// 指针区分“未声明（回退平台硬编码默认）”与“显式空（仅 audit_ingest）”。
	AllowedServiceBindings *[]string `yaml:"allowed_service_bindings"`
}

type productionSubsystemRuntimeManifest struct {
	RequiredInfrastructureKeys []string                                 `yaml:"required_infrastructure_keys"`
	Files                      []productionSubsystemRuntimeFileManifest `yaml:"files"`
}

type productionSubsystemRuntimeFileManifest struct {
	Path                  string            `yaml:"path"`
	TemplatePath          string            `yaml:"template_path"`
	ComposeEnvironmentKey string            `yaml:"compose_environment_key"`
	Bindings              map[string]string `yaml:"bindings"`
	Values                map[string]string `yaml:"values"`
	RequiredExistingKeys  []string          `yaml:"required_existing_keys"`
	GeneratedKeys         []string          `yaml:"generated_keys"`
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
	// 清单读取是生产安全边界：目录、文件大小、符号链接和 YAML 结构均在这里收紧，
	// 后续 Provision 只消费已规范化的内存对象，不接受请求带来的路径或命令。
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
	runtimeEnvironmentOwners := make(map[string]struct {
		environment string
		sourcePath  string
	})
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
		for _, runtimeFile := range profile.Manifest.Runtime.Files {
			// 同一应用的 dev/prod 目标绝不能共写一个 runtime 文件。不同应用在同一
			// 环境内可以通过审核清单共享文件（例如 Portal 为 CRM 写入映射凭据）。
			ownerKey := profile.Manifest.Application.Code + "\x00" + runtimeFile.Path
			if owner, exists := runtimeEnvironmentOwners[ownerKey]; exists && owner.environment != profile.Manifest.Application.Environment {
				return nil, "", fmt.Errorf("production subsystem runtime %s is shared by %s and %s environments in %s and %s",
					runtimeFile.Path, owner.environment, profile.Manifest.Application.Environment, owner.sourcePath, path)
			}
			runtimeEnvironmentOwners[ownerKey] = struct {
				environment string
				sourcePath  string
			}{environment: profile.Manifest.Application.Environment, sourcePath: path}
		}
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
	// 规范化同时消除路径别名和空白差异；校验失败必须在进入 Docker 前返回，避免审核清单
	// 通过一个相对路径或未声明环境键扩大实际写入范围。
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
	if app.InitialAdminRoles != nil {
		if len(*app.InitialAdminRoles) > 16 {
			return errors.New("initial admin roles must be at most 16")
		}
		for _, role := range *app.InitialAdminRoles {
			if !validProductionTargetCode(role, 64) {
				return errors.New("initial admin role is invalid")
			}
		}
	}
	if app.AllowedServiceBindings != nil {
		if len(*app.AllowedServiceBindings) > 32 {
			return errors.New("allowed service bindings must be at most 32")
		}
		for _, purpose := range *app.AllowedServiceBindings {
			if !validProductionTargetCode(purpose, 64) {
				return errors.New("allowed service binding purpose is invalid")
			}
		}
	}

	runtime := &manifest.Runtime
	runtime.RequiredInfrastructureKeys = normalizedProductionEnvironmentKeys(runtime.RequiredInfrastructureKeys)
	if runtime.RequiredInfrastructureKeys == nil || len(runtime.Files) == 0 || len(runtime.Files) > 32 {
		return errors.New("runtime file policy is invalid")
	}
	seenPaths := make(map[string]struct{}, len(runtime.Files))
	seenComposeKeys := make(map[string]struct{}, len(runtime.Files))
	for index := range runtime.Files {
		file := &runtime.Files[index]
		file.Path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file.Path)))
		file.TemplatePath = normalizedProductionTemplatePath(file.TemplatePath)
		file.ComposeEnvironmentKey = strings.TrimSpace(file.ComposeEnvironmentKey)
		if file.Path == "." || filepath.IsAbs(file.Path) || file.Path == "runtime" || !strings.HasPrefix(file.Path, "runtime/") ||
			strings.Contains(file.Path, "../") || !validEnvironmentKey(file.ComposeEnvironmentKey) || file.TemplatePath == "." {
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
		for key, source := range file.Bindings {
			source = strings.TrimSpace(source)
			if !validEnvironmentKey(key) || !validProductionBindingSource(source) ||
				!productionServiceBindingAllowed(app, source) {
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
		if code, exists := file.Values["PLATFORM_APPLICATION_CODE"]; exists && strings.TrimSpace(code) != app.Code {
			return errors.New("runtime application code does not match target")
		}
		if environment, exists := file.Values["PLATFORM_ENVIRONMENT_CODE"]; exists && strings.ToLower(strings.TrimSpace(environment)) != app.Environment {
			return errors.New("runtime environment code does not match target")
		}
		file.RequiredExistingKeys = normalizedProductionEnvironmentKeys(file.RequiredExistingKeys)
		if file.RequiredExistingKeys == nil {
			return errors.New("runtime required-existing key is invalid")
		}
		file.GeneratedKeys = normalizedProductionEnvironmentKeys(file.GeneratedKeys)
		if file.GeneratedKeys == nil {
			return errors.New("runtime generated key is invalid")
		}
		if len(file.GeneratedKeys) > 0 && file.TemplatePath == "" {
			return errors.New("runtime generated keys require a reviewed template")
		}
		requiredKeys := make(map[string]struct{}, len(file.RequiredExistingKeys))
		for _, key := range file.RequiredExistingKeys {
			requiredKeys[key] = struct{}{}
		}
		for _, key := range file.GeneratedKeys {
			if !validProductionGeneratedEnvironmentKey(key) {
				return errors.New("runtime generated key must be a base64 key or pepper")
			}
			if _, duplicate := file.Bindings[key]; duplicate {
				return errors.New("runtime generated key overlaps a managed binding")
			}
			if _, duplicate := file.Values[key]; duplicate {
				return errors.New("runtime generated key overlaps a fixed value")
			}
			if _, duplicate := requiredKeys[key]; duplicate {
				return errors.New("runtime generated key overlaps a required-existing key")
			}
		}
		if file.TemplatePath == "" && len(file.Bindings) == 0 && len(file.Values) == 0 &&
			len(file.RequiredExistingKeys) == 0 && len(file.GeneratedKeys) == 0 {
			return errors.New("runtime file policy is empty")
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

// normalizedProductionTemplatePath 允许随发布包增加新的受控模板，而不要求为每个
// 子系统修改 Agent 代码。空值表示兼容既有的预置 runtime 文件；非空值必须是部署根
// 内的 *.env.example 相对路径，运行时还会再次拒绝符号链接和越界解析。
func normalizedProductionTemplatePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := filepath.ToSlash(filepath.Clean(value))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
		strings.Contains(cleaned, "\x00") || !strings.HasSuffix(cleaned, ".env.example") {
		return "."
	}
	return cleaned
}

func validProductionGeneratedEnvironmentKey(value string) bool {
	return strings.HasSuffix(value, "_KEY_BASE64") || strings.HasSuffix(value, "_PEPPER_BASE64")
}

func validProductionBindingSource(source string) bool {
	switch source {
	case "issuer", "client_id", "client_secret", "redirect_uri", "logged_out_url", "public_url", "public_url_no_trailing_slash", "public_origin",
		"tenant_id", "application_id", "application_code", "environment", "path_prefix", "upstream_url", "cookie_secure",
		"catalog_publisher_client_id", "catalog_publisher_client_secret", "issuer_security_center_url", "authorization_context_url":
		return true
	}
	parts := strings.Split(source, ".")
	return len(parts) == 3 && parts[0] == "service" && validProductionTargetCode(parts[1], 64) && (parts[2] == "client_id" || parts[2] == "client_secret")
}

// productionServiceBindingAllowed 决定某服务用途绑定是否被允许。B2 解耦：清单显式声明
// allowed_service_bindings 时按清单校验；未声明时回退到平台硬编码默认（保证既有行为不变）。
// audit_ingest 是所有接入环境的基线能力，恒允许。
func productionServiceBindingAllowed(app *productionSubsystemApplicationManifest, source string) bool {
	parts := strings.Split(source, ".")
	if len(parts) != 3 || parts[0] != "service" {
		return true
	}
	purpose := parts[1]
	if purpose == application.ServiceCredentialAuditIngest {
		return true
	}
	if app.AllowedServiceBindings != nil {
		if !stringSliceContains(*app.AllowedServiceBindings, purpose) {
			return false
		}
		defaultPurposes := hardcodedProductionServiceBindingPurposes(app.Code)
		if !stringSliceContains(defaultPurposes, purpose) {
			// 清单比平台默认多放了某个用途：允许（清单是权威），但记录告警便于审计。
			slog.Warn("subsystem service binding declared beyond platform default",
				"application_code", app.Code, "purpose", purpose)
		}
		return true
	}
	return stringSliceContains(hardcodedProductionServiceBindingPurposes(app.Code), purpose)
}

// hardcodedProductionServiceBindingPurposes 是平台内置默认用途集合（不含恒允许的 audit_ingest）。
// 清单未声明 allowed_service_bindings 时使用，保证既有子系统行为不变。
func hardcodedProductionServiceBindingPurposes(applicationCode string) []string {
	switch strings.TrimSpace(applicationCode) {
	case "customer_and_opportunity":
		return []string{
			application.ServiceCredentialOwnerDirectoryRead,
			application.ServiceCredentialContractSummaryRead,
			application.ServiceCredentialContractOpportunitySignedWrite,
		}
	case "customer_portal":
		return []string{
			application.ServiceCredentialExternalUserProvision,
			application.ServiceCredentialApplicationRoleAssign,
			application.ServiceCredentialApplicationRoleRevoke,
			application.ServiceCredentialPortalMappingProvision,
			application.ServiceCredentialPortalMappingDisable,
			application.ServiceCredentialPortalInviteVerify,
		}
	}
	return nil
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
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
		target := application.SubsystemProvisioningTarget{
			ApplicationCode: app.Code, ApplicationName: app.Name, Description: app.Description,
			Environment: app.Environment, UpstreamURL: app.UpstreamURL, PathPrefix: app.PathPrefix, ClientType: app.ClientType,
		}
		if app.InitialAdminRoles != nil {
			target.InitialAdminRoles = append([]string(nil), (*app.InitialAdminRoles)...)
		}
		if app.AllowedServiceBindings != nil {
			target.AllowedServiceBindings = append([]string(nil), (*app.AllowedServiceBindings)...)
		}
		capabilities.Targets = append(capabilities.Targets, target)
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
