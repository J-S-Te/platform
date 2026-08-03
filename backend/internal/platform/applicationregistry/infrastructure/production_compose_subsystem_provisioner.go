package infrastructure

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

// ProductionComposeSubsystemProvisionerConfig 固定生产部署根、租户和 Compose 入口。
// 可接入目标由 ProfilesDirectory 中随发布包审核的 YAML 清单提供，不再为每个新子系统
// 增加一组环境变量，也不允许 HTTP 请求指定文件、镜像、服务或命令。
type ProductionComposeSubsystemProvisionerConfig struct {
	Enabled           bool
	DeployRoot        string
	ProfilesDirectory string
	RuntimeEnvPath    string
	ReleaseEnvPath    string
	ComposeFile       string
	AllowedTenantID   string
	ComposeProject    string
	DockerBinary      string
	Timeout           time.Duration
}

// ProductionComposeSubsystemProvisioner 是按 application_code/environment 分派的生产
// 执行器。所有目标都来自本地审核清单；未知目标在访问 Docker 或运行时文件前即被拒绝。
type ProductionComposeSubsystemProvisioner struct {
	enabled      bool
	targets      map[string]*productionComposeTarget
	capabilities application.SubsystemProvisioningCapabilities
}

type productionComposeTargetConfig struct {
	DeployRoot            string
	RuntimeEnvPath        string
	ReleaseEnvPath        string
	ComposeFile           string
	AllowedTenantID       string
	ComposeProject        string
	DockerBinary          string
	Timeout               time.Duration
	Profile               productionSubsystemProfile
	RuntimeBootstrapFiles []productionSubsystemRuntimeFileManifest
}

type productionComposeTarget struct {
	config productionComposeTargetConfig
	runner subsystemCommandRunner
	mutex  sync.Mutex
}

// NewProductionComposeSubsystemProvisioner 加载并验证所有审核清单后构造 Agent 执行器。
func NewProductionComposeSubsystemProvisioner(config ProductionComposeSubsystemProvisionerConfig) (*ProductionComposeSubsystemProvisioner, error) {
	return newProductionComposeSubsystemProvisioner(config, execSubsystemCommandRunner{})
}

func newProductionComposeSubsystemProvisioner(config ProductionComposeSubsystemProvisionerConfig, runner subsystemCommandRunner) (*ProductionComposeSubsystemProvisioner, error) {
	if runner == nil {
		return nil, errors.New("production subsystem command runner is required")
	}
	root, err := canonicalProductionDeployRoot(config.DeployRoot)
	if err != nil {
		return nil, err
	}
	config.DeployRoot = root
	config.AllowedTenantID = strings.TrimSpace(config.AllowedTenantID)
	if config.AllowedTenantID == "" || strings.ContainsAny(config.AllowedTenantID, "\r\n\x00") {
		return nil, errors.New("production subsystem allowed tenant is required")
	}
	config.RuntimeEnvPath = productionConfigPath(root, config.RuntimeEnvPath, ".env")
	config.ReleaseEnvPath = productionConfigPath(root, config.ReleaseEnvPath, ".release.env")
	config.ComposeFile = productionConfigPath(root, config.ComposeFile, "compose.yaml")
	config.ComposeProject = strings.TrimSpace(config.ComposeProject)
	if config.ComposeProject == "" {
		config.ComposeProject = "basic-platform-production"
	}
	if !validProductionComposeService(config.ComposeProject) {
		return nil, errors.New("production Compose project name is invalid")
	}
	config.DockerBinary = strings.TrimSpace(config.DockerBinary)
	if config.DockerBinary == "" {
		config.DockerBinary = "docker"
	}
	if strings.ContainsAny(config.DockerBinary, "/\\\r\n\x00") {
		return nil, errors.New("production Docker binary must be a command name")
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Minute
	}

	profiles, _, err := loadProductionSubsystemProfiles(root, config.ProfilesDirectory)
	if err != nil {
		return nil, err
	}
	runtimeBootstrapFiles, err := productionRuntimeBootstrapFiles(profiles)
	if err != nil {
		return nil, err
	}
	provisioner := &ProductionComposeSubsystemProvisioner{
		enabled:      config.Enabled,
		targets:      make(map[string]*productionComposeTarget, len(profiles)),
		capabilities: productionSubsystemCapabilities(profiles),
	}
	provisioner.capabilities.Enabled = config.Enabled
	for _, profile := range profiles {
		target := &productionComposeTarget{config: productionComposeTargetConfig{
			DeployRoot: root, RuntimeEnvPath: config.RuntimeEnvPath, ReleaseEnvPath: config.ReleaseEnvPath,
			ComposeFile: config.ComposeFile, AllowedTenantID: config.AllowedTenantID,
			ComposeProject: config.ComposeProject, DockerBinary: config.DockerBinary,
			Timeout: config.Timeout, Profile: profile, RuntimeBootstrapFiles: runtimeBootstrapFiles,
		}, runner: runner}
		key := productionSubsystemTargetKey(profile.Manifest.Application.Code, profile.Manifest.Application.Environment)
		provisioner.targets[key] = target
	}
	return provisioner, nil
}

func productionConfigPath(root, configured, fallback string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		configured = filepath.Join(root, fallback)
	} else if !filepath.IsAbs(configured) {
		configured = filepath.Join(root, configured)
	}
	return filepath.Clean(configured)
}

// productionRuntimeBootstrapFiles 汇总所有已审核目标的 runtime 文件，只用于首次创建和
// 权限修复。不同目标对同一文件可以声明不同的受管键，但模板和 Compose 环境变量必须
// 一致；实际凭据校验和写入仍只使用当前目标自己的清单，避免把一个应用的凭据写给另一个。
func productionRuntimeBootstrapFiles(profiles []productionSubsystemProfile) ([]productionSubsystemRuntimeFileManifest, error) {
	files := make([]productionSubsystemRuntimeFileManifest, 0)
	byPath := make(map[string]int)
	for _, profile := range profiles {
		for _, runtimeFile := range profile.Manifest.Runtime.Files {
			index, exists := byPath[runtimeFile.Path]
			if !exists {
				byPath[runtimeFile.Path] = len(files)
				files = append(files, productionSubsystemRuntimeFileManifest{
					Path: runtimeFile.Path, TemplatePath: runtimeFile.TemplatePath,
					ComposeEnvironmentKey: runtimeFile.ComposeEnvironmentKey,
				})
				continue
			}
			existing := &files[index]
			if existing.ComposeEnvironmentKey != runtimeFile.ComposeEnvironmentKey {
				return nil, errors.New("production runtime file Compose environment key is inconsistent across profiles")
			}
			if existing.TemplatePath == "" {
				existing.TemplatePath = runtimeFile.TemplatePath
			} else if runtimeFile.TemplatePath != "" && existing.TemplatePath != runtimeFile.TemplatePath {
				return nil, errors.New("production runtime file template is inconsistent across profiles")
			}
		}
	}
	return files, nil
}

// Capabilities 返回防御性副本，供测试和未来的 Agent 诊断使用。API 侧独立加载同一清单，
// 不通过 Unix Socket获取宿主机路径或其他敏感配置。
func (provisioner *ProductionComposeSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	capabilities := provisioner.capabilities
	capabilities.SupportedApplicationCodes = append([]string(nil), capabilities.SupportedApplicationCodes...)
	capabilities.SupportedEnvironments = append([]string(nil), capabilities.SupportedEnvironments...)
	capabilities.Targets = append([]application.SubsystemProvisioningTarget(nil), capabilities.Targets...)
	return capabilities
}

func (provisioner *ProductionComposeSubsystemProvisioner) target(applicationCode, environment string) (*productionComposeTarget, error) {
	if provisioner == nil || !provisioner.enabled {
		return nil, provisioningError("production subsystem deployment is disabled")
	}
	target, ok := provisioner.targets[productionSubsystemTargetKey(applicationCode, environment)]
	if !ok {
		return nil, provisioningError("production subsystem target is not allowed")
	}
	return target, nil
}

func (provisioner *ProductionComposeSubsystemProvisioner) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	target, err := provisioner.target(input.ApplicationCode, input.Environment)
	if err != nil {
		return err
	}
	return target.Preflight(ctx, input)
}

func (provisioner *ProductionComposeSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	target, err := provisioner.target(input.ApplicationCode, input.Environment)
	if err != nil {
		return err
	}
	return target.Provision(ctx, input)
}

func (provisioner *ProductionComposeSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	target, err := provisioner.target(input.ApplicationCode, input.Environment)
	if err != nil {
		return err
	}
	return target.Update(ctx, input)
}

func (provisioner *ProductionComposeSubsystemProvisioner) Teardown(ctx context.Context, tenantID, applicationCode, environment string) error {
	target, err := provisioner.target(applicationCode, environment)
	if err != nil {
		return err
	}
	return target.Teardown(ctx, tenantID)
}

// Preflight 在创建不可恢复的 OAuth 明文前验证租户、目标、文件权限、不可变镜像和
// Compose 配置。请求中的公开字段必须与审核清单完全一致。
func (target *productionComposeTarget) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	if err := target.validateTenant(input.TenantID); err != nil {
		return err
	}
	if err := target.validatePreflightInput(input); err != nil {
		return err
	}
	if err := target.validateDeploymentFiles(true); err != nil {
		return err
	}
	checkContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := target.runner.Run(checkContext, target.config.DeployRoot, os.Environ(), target.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return provisioningError("Docker service is unavailable")
	}
	if err := target.runCompose(checkContext, "config", "--quiet"); err != nil {
		return provisioningError("production Compose configuration is invalid")
	}
	return nil
}

// Provision 串行写入清单允许的键，并在与 CI/CD 共用的文件锁内执行固定部署步骤。
func (target *productionComposeTarget) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	if err := target.validateProvisioningInput(input); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, target.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(target.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)

	if err := target.writeRuntimeConfiguration(input); err != nil {
		return err
	}
	return target.deployLocked(operationContext)
}

// Update 只重用已落盘配置，不尝试从数据库恢复 OAuth 明文或隐式轮换密钥。
func (target *productionComposeTarget) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	if err := target.validateTenant(input.TenantID); err != nil {
		return err
	}
	if !target.matches(input.ApplicationCode, input.Environment) {
		return provisioningError("production subsystem deployment request is invalid")
	}
	if err := target.validateDeploymentFiles(false); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, target.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(target.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)
	return target.deployLocked(operationContext)
}

// Teardown 仅停止清单列出的运行服务，保留数据库、备份、运行时秘密和发布指针。
func (target *productionComposeTarget) Teardown(ctx context.Context, tenantID string) error {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	if err := target.validateTenant(tenantID); err != nil {
		return err
	}
	if err := target.validateDeploymentFiles(false); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, target.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(target.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)
	arguments := []string{"stop"}
	arguments = append(arguments, target.config.Profile.Manifest.Compose.TeardownServices...)
	if err := target.runCompose(operationContext, arguments...); err != nil {
		return provisioningError("stop production subsystem services")
	}
	return nil
}

func (target *productionComposeTarget) deployLocked(ctx context.Context) error {
	compose := target.config.Profile.Manifest.Compose
	if len(compose.DependencyServices) > 0 {
		arguments := []string{"up", "-d", "--wait", "--wait-timeout", "240"}
		arguments = append(arguments, compose.DependencyServices...)
		if err := target.runCompose(ctx, arguments...); err != nil {
			return provisioningError("start production subsystem dependencies")
		}
	}
	if compose.Database != nil {
		backupName := target.config.Profile.Manifest.Application.Code + "-onboarding-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".sql"
		if err := os.MkdirAll(filepath.Join(target.config.DeployRoot, "backups"), 0o750); err != nil {
			return provisioningError("prepare production subsystem backup")
		}
		if err := target.runCompose(ctx, "exec", "-T", compose.Database.Service, "sh", "-c",
			`set -eu; umask 077; exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$2" > "/backups/$1"`,
			"_", backupName, compose.Database.Name); err != nil {
			return provisioningError("backup production subsystem database")
		}
	}
	if compose.MigrateService != "" {
		if err := target.runCompose(ctx, "run", "--rm", "--no-deps", compose.MigrateService); err != nil {
			return provisioningError("migrate production subsystem database")
		}
	}
	arguments := []string{"up", "-d", "--wait", "--wait-timeout", "240", "--force-recreate", "--no-deps"}
	arguments = append(arguments, compose.RuntimeServices...)
	if err := target.runCompose(ctx, arguments...); err != nil {
		return provisioningError("start production subsystem services")
	}
	return nil
}

func (target *productionComposeTarget) runCompose(ctx context.Context, arguments ...string) error {
	prefix := []string{
		"compose", "--project-name", target.config.ComposeProject,
		"--project-directory", target.config.DeployRoot,
		"--env-file", target.config.RuntimeEnvPath,
		"--env-file", target.config.ReleaseEnvPath,
		"--file", target.config.ComposeFile,
	}
	for _, profile := range target.config.Profile.Manifest.Compose.Profiles {
		prefix = append(prefix, "--profile", profile)
	}
	runnerEnvironment := append([]string{}, os.Environ()...)
	runnerEnvironment = append(runnerEnvironment, "BASIC_PLATFORM_RUNTIME_ENV_FILE="+target.config.RuntimeEnvPath)
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		runnerEnvironment = append(runnerEnvironment, runtimeFile.ComposeEnvironmentKey+"="+filepath.Join(target.config.DeployRoot, filepath.FromSlash(runtimeFile.Path)))
	}
	return target.runner.Run(ctx, target.config.DeployRoot, runnerEnvironment, target.config.DockerBinary, append(prefix, arguments...)...)
}

func (target *productionComposeTarget) writeRuntimeConfiguration(input application.SubsystemProvisioningInput) error {
	type runtimeEnvironmentUpdate struct {
		path   string
		values map[string]string
	}
	updates := make([]runtimeEnvironmentUpdate, 0, len(target.config.Profile.Manifest.Runtime.Files))
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		path := filepath.Join(target.config.DeployRoot, filepath.FromSlash(runtimeFile.Path))
		generatedValues, err := productionGeneratedEnvironmentValues(path, runtimeFile.GeneratedKeys)
		if err != nil {
			return err
		}
		values := make(map[string]string, len(runtimeFile.Bindings)+len(runtimeFile.Values)+len(generatedValues))
		for key, value := range runtimeFile.Values {
			values[key] = value
		}
		for key, value := range generatedValues {
			values[key] = value
		}
		for key, source := range runtimeFile.Bindings {
			value, resolveErr := resolveProductionBinding(input, source)
			if resolveErr != nil {
				return resolveErr
			}
			values[key] = value
		}
		updates = append(updates, runtimeEnvironmentUpdate{path: path, values: values})
	}
	for _, update := range updates {
		if err := updateProductionSubsystemEnvironment(update.path, update.values); err != nil {
			return provisioningError("write production subsystem runtime configuration")
		}
	}
	return nil
}

func resolveProductionBinding(input application.SubsystemProvisioningInput, source string) (string, error) {
	issuer := strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	switch source {
	case "issuer", "public_origin":
		return issuer, nil
	case "client_id":
		return input.ClientID, nil
	case "client_secret":
		return input.ClientSecret, nil
	case "redirect_uri":
		return input.RedirectURI, nil
	case "logged_out_url":
		return strings.TrimRight(input.PublicURL, "/") + "/logged-out", nil
	case "public_url":
		return input.PublicURL, nil
	case "public_url_no_trailing_slash":
		return strings.TrimRight(input.PublicURL, "/"), nil
	case "tenant_id":
		return input.TenantID, nil
	case "application_id":
		return input.ApplicationID, nil
	case "application_code":
		return input.ApplicationCode, nil
	case "environment":
		return input.Environment, nil
	case "path_prefix":
		return input.PathPrefix, nil
	case "upstream_url":
		return input.UpstreamURL, nil
	case "cookie_secure":
		parsed := mustParseURL(issuer)
		return booleanEnvironmentValue(parsed != nil && strings.EqualFold(parsed.Scheme, "https")), nil
	case "catalog_publisher_client_id":
		return input.CatalogPublisherClientID, nil
	case "catalog_publisher_client_secret":
		return input.CatalogPublisherClientSecret, nil
	case "issuer_security_center_url":
		return issuer + "/settings/security", nil
	}
	parts := strings.Split(source, ".")
	if len(parts) == 3 && parts[0] == "service" {
		credential, ok := input.ServiceCredential(parts[1])
		if !ok {
			return "", provisioningError("production subsystem service credential is incomplete")
		}
		switch parts[2] {
		case "client_id":
			return credential.OAuthClient.ClientID, nil
		case "client_secret":
			return credential.PlaintextSecret, nil
		}
	}
	return "", provisioningError("production subsystem runtime binding is unsupported")
}

func (target *productionComposeTarget) validateDeploymentFiles(requireWritableEnvironment bool) error {
	root, err := canonicalProductionDeployRoot(target.config.DeployRoot)
	if err != nil {
		return provisioningError("production deployment directory is unavailable")
	}
	// runtime 文件属于 Agent 的受控输出：首次接入可从随发布包审核的模板初始化，
	// 已有文件只收紧权限且绝不整体覆盖。清单之外的文件和环境变量不会参与校验。
	runtimeFilesToPrepare := target.config.Profile.Manifest.Runtime.Files
	if requireWritableEnvironment {
		runtimeFilesToPrepare = target.config.RuntimeBootstrapFiles
	}
	for _, runtimeFile := range runtimeFilesToPrepare {
		if err := ensureProductionRuntimeFile(root, runtimeFile, requireWritableEnvironment); err != nil {
			return err
		}
	}
	paths := []string{target.config.RuntimeEnvPath, target.config.ReleaseEnvPath, target.config.ComposeFile}
	for _, path := range paths {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil || resolved != filepath.Clean(path) || !pathWithinRoot(root, resolved) {
			return provisioningError("production deployment file is unavailable")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return provisioningError("production deployment files have unsafe permissions")
		}
	}
	if info, statErr := os.Stat(target.config.RuntimeEnvPath); statErr != nil || info.Mode().Perm()&0o077 != 0 {
		return provisioningError("production infrastructure environment permissions must be 0600")
	}
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		path := filepath.Join(root, filepath.FromSlash(runtimeFile.Path))
		if err := validateProductionRequiredEnvironmentKeys(path, runtimeFile.RequiredExistingKeys, "production subsystem runtime secrets are incomplete"); err != nil {
			return err
		}
		if requireWritableEnvironment {
			temporary, createErr := os.CreateTemp(filepath.Dir(path), ".provisioning-write-check.*")
			if createErr != nil {
				return provisioningError("production subsystem environment is not writable")
			}
			temporaryPath := temporary.Name()
			closeErr := temporary.Close()
			removeErr := os.Remove(temporaryPath)
			if closeErr != nil || removeErr != nil {
				return provisioningError("production subsystem environment is not writable")
			}
		}
	}
	if err := validateProductionRequiredEnvironmentKeys(target.config.RuntimeEnvPath, target.config.Profile.Manifest.Runtime.RequiredInfrastructureKeys, "production infrastructure secrets are incomplete"); err != nil {
		return err
	}
	for _, imageKey := range target.config.Profile.Manifest.Compose.ReleaseImageKeys {
		if err := validateProductionReleaseImage(target.config.ReleaseEnvPath, imageKey); err != nil {
			return err
		}
	}
	return nil
}

// ensureProductionRuntimeFile 将人工步骤压缩为可重复的 Agent 操作：缺失时复制审核模板，
// 已存在时保留全部内容，仅把权限收紧到 0600。符号链接、越界路径和非普通文件仍拒绝，
// 防止清单更新被利用为宿主机任意文件写入。
func ensureProductionRuntimeFile(root string, runtimeFile productionSubsystemRuntimeFileManifest, allowInitialize bool) error {
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(runtimeFile.Path)))
	if !pathWithinRoot(root, path) {
		return provisioningError("production subsystem runtime environment path is invalid")
	}
	if err := ensureProductionRuntimeDirectory(root, filepath.Dir(path), allowInitialize); err != nil {
		return provisioningError("production subsystem runtime directory is unavailable")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !allowInitialize || strings.TrimSpace(runtimeFile.TemplatePath) == "" {
			return provisioningError("production subsystem runtime environment is unavailable")
		}
		if err := initializeProductionRuntimeFile(root, path, runtimeFile.TemplatePath); err != nil {
			return provisioningError("production subsystem runtime template is unavailable")
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return provisioningError("production subsystem runtime environment is unavailable")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return provisioningError("production subsystem runtime environment permissions cannot be tightened")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || !pathWithinRoot(root, resolved) {
		return provisioningError("production subsystem runtime environment path is invalid")
	}
	info, err = os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return provisioningError("production subsystem runtime environment permissions must be 0600")
	}
	return nil
}

func initializeProductionRuntimeFile(root, destinationPath, templateRelativePath string) error {
	templatePath := filepath.Clean(filepath.Join(root, filepath.FromSlash(templateRelativePath)))
	if !pathWithinRoot(root, templatePath) {
		return errors.New("runtime template is outside deployment root")
	}
	resolvedTemplate, err := filepath.EvalSymlinks(templatePath)
	if err != nil || resolvedTemplate != templatePath || !pathWithinRoot(root, resolvedTemplate) {
		return errors.New("runtime template path is unavailable")
	}
	templateInfo, err := os.Lstat(resolvedTemplate)
	if err != nil || templateInfo.Mode()&os.ModeSymlink != 0 || !templateInfo.Mode().IsRegular() || templateInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("runtime template metadata is unsafe")
	}
	if err := ensureProductionRuntimeDirectory(root, filepath.Dir(destinationPath), true); err != nil {
		return err
	}

	source, err := os.Open(resolvedTemplate)
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destinationPath), ".runtime-env.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = io.Copy(temporary, source)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if stat, ok := templateInfo.Sys().(*syscall.Stat_t); ok {
		temporaryInfo, statErr := os.Stat(temporaryPath)
		if statErr != nil {
			return statErr
		}
		if temporaryStat, ok := temporaryInfo.Sys().(*syscall.Stat_t); ok &&
			(temporaryStat.Uid != stat.Uid || temporaryStat.Gid != stat.Gid) {
			if err := os.Chown(temporaryPath, int(stat.Uid), int(stat.Gid)); err != nil {
				return err
			}
		}
	}
	// 硬链接提供“不覆盖已存在文件”的原子语义；并发预检时先完成者成为唯一初始化结果。
	if err := os.Link(temporaryPath, destinationPath); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(destinationPath))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensureProductionRuntimeDirectory(root, directory string, allowCreate bool) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("runtime directory is outside deployment root")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return errors.New("runtime directory component is invalid")
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if !allowCreate {
				return statErr
			}
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			return errors.New("runtime directory metadata is unsafe")
		}
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr != nil || resolved != current || !pathWithinRoot(root, resolved) {
			return errors.New("runtime directory path is unsafe")
		}
	}
	return nil
}

// productionGeneratedEnvironmentValues 只为清单声明的长期 base64 密钥填充占位值。
// 已有合法值永远不返回为 replacement，因此接入重试、更新和 Agent 重启都不会轮换密钥。
func productionGeneratedEnvironmentValues(path string, keys []string) (map[string]string, error) {
	generated := make(map[string]string)
	if len(keys) == 0 {
		return generated, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, provisioningError("production subsystem runtime environment is unavailable")
	}
	values := parseEnvironmentValues(string(content))
	for _, key := range keys {
		value := strings.TrimSpace(values[key])
		if !productionEnvironmentValueMissing(value) {
			if !validProductionGeneratedEnvironmentValue(key, value) {
				return nil, provisioningError("production subsystem generated runtime secret is invalid")
			}
			continue
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, provisioningError("generate production subsystem runtime secret")
		}
		generated[key] = base64.StdEncoding.EncodeToString(secret)
	}
	return generated, nil
}

func validProductionGeneratedEnvironmentValue(key, value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	if strings.Contains(key, "HMAC") || strings.Contains(key, "PEPPER") {
		return len(decoded) >= 32
	}
	return len(decoded) == 32
}

func productionEnvironmentValueMissing(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || strings.HasPrefix(value, "REPLACE_WITH_") || strings.HasPrefix(value, "PENDING_")
}

func validateProductionRequiredEnvironmentKeys(path string, keys []string, message string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return provisioningError("production environment is unavailable")
	}
	values := parseEnvironmentValues(string(content))
	for _, key := range keys {
		if productionEnvironmentValueMissing(values[key]) {
			return provisioningError(message)
		}
	}
	return nil
}

func validateProductionReleaseImage(path, key string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return provisioningError("production release environment is unavailable")
	}
	value := strings.TrimSpace(parseEnvironmentValues(string(content))[key])
	marker := "@sha256:"
	index := strings.LastIndex(value, marker)
	if index <= 0 || len(value[index+len(marker):]) != 64 {
		return provisioningError("production subsystem image must use an immutable digest")
	}
	for _, character := range value[index+len(marker):] {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return provisioningError("production subsystem image must use an immutable digest")
		}
	}
	return nil
}

func parseEnvironmentValues(content string) map[string]string {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "=")
		if found && validEnvironmentKey(strings.TrimSpace(key)) {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	return values
}

func (target *productionComposeTarget) validateProvisioningInput(input application.SubsystemProvisioningInput) error {
	if !target.matches(input.ApplicationCode, input.Environment) {
		return provisioningError("production subsystem deployment request is invalid")
	}
	if err := target.validateTenant(input.TenantID); err != nil {
		return err
	}
	app := target.config.Profile.Manifest.Application
	issuer := strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	expectedPublicURL := issuer + app.PathPrefix + "/"
	expectedRedirectURI := issuer + app.PathPrefix + "/auth/callback"
	if mustParseURL(issuer) == nil || strings.TrimSpace(input.ApplicationID) == "" || strings.TrimSpace(input.ClientID) == "" ||
		strings.TrimSpace(input.ClientSecret) == "" || strings.TrimSpace(input.CatalogPublisherClientID) == "" ||
		strings.TrimSpace(input.CatalogPublisherClientSecret) == "" || input.PathPrefix != app.PathPrefix ||
		strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/") != app.UpstreamURL || input.PublicURL != expectedPublicURL || input.RedirectURI != expectedRedirectURI {
		return provisioningError("production subsystem integration values are inconsistent")
	}
	// 先解析所有映射，确保缺少任一用途凭据时不会先写入部分运行时文件。
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		for _, source := range runtimeFile.Bindings {
			value, err := resolveProductionBinding(input, source)
			if err != nil || !validEnvironmentValue(value) {
				return provisioningError("production subsystem integration credential is incomplete")
			}
		}
	}
	return target.validateDeploymentFiles(true)
}

func (target *productionComposeTarget) validateTenant(tenantID string) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || tenantID != target.config.AllowedTenantID || strings.ContainsAny(tenantID, "\r\n\x00") {
		return provisioningError("production subsystem tenant is not allowed")
	}
	return nil
}

func (target *productionComposeTarget) validatePreflightInput(input application.SubsystemPreflightInput) error {
	app := target.config.Profile.Manifest.Application
	issuer := strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	if mustParseURL(issuer) == nil || !target.matches(input.ApplicationCode, input.Environment) ||
		strings.ToLower(strings.TrimSpace(input.ClientType)) != app.ClientType ||
		strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/") != issuer ||
		strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/") != app.UpstreamURL ||
		strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/") != app.PathPrefix {
		return provisioningError("production subsystem preflight values are inconsistent")
	}
	return nil
}

func (target *productionComposeTarget) matches(applicationCode, environment string) bool {
	app := target.config.Profile.Manifest.Application
	return strings.ToLower(strings.TrimSpace(applicationCode)) == app.Code && strings.ToLower(strings.TrimSpace(environment)) == app.Environment
}

func validProductionTargetCode(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

func validProductionPathPrefix(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && !strings.HasSuffix(value, "/") && !strings.Contains(value, "//") &&
		!strings.Contains(value, "..") && !strings.ContainsAny(value, "?#\\\r\n\x00")
}

func validProductionUpstreamURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizedProductionServices(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validProductionComposeService(value) {
			return nil
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validProductionComposeService(value string) bool {
	return validProductionTargetCode(value, 64)
}

func validProductionDatabaseName(value string) bool {
	return validProductionTargetCode(value, 64)
}

func validProductionReleaseImageKey(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || index > 0 && (character >= '0' && character <= '9' || character == '_') {
			continue
		}
		return false
	}
	return true
}

func mustParseURL(value string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil
	}
	return parsed
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func acquireProvisioningFileLock(ctx context.Context, path string) (*os.File, error) {
	// Linux/macOS 上使用同一路径 flock，与 deploy-service.sh 的发布锁跨进程互斥；
	// 非阻塞轮询让请求超时或取消可以及时结束，而不是永久卡住 Agent。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func releaseProvisioningFileLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
