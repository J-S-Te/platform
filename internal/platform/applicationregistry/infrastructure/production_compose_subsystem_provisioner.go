package infrastructure

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const productionAuthorizationContextURL = "http://platform-api:8080/oauth2/authorization-context"

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
	// AllowPlaceholderDatabaseCredentials 仅用于测试服务器：为 true 时跳过子系统
	// 数据库凭据校验，允许 Compose 在首次创建空数据库时使用占位密码完成接入。
	// 生产环境必须保持 false，避免已有持久化数据库因密码未初始化而不可控。
	AllowPlaceholderDatabaseCredentials bool
}

// ProductionComposeSubsystemProvisioner 是按 application_code/environment 分派的生产
// 执行器。所有目标都来自本地审核清单；未知目标在访问 Docker 或运行时文件前即被拒绝。
type ProductionComposeSubsystemProvisioner struct {
	enabled      bool
	targets      map[string]*productionComposeTarget
	capabilities application.SubsystemProvisioningCapabilities
	dockerBinary string
}

type productionComposeTargetConfig struct {
	DeployRoot                          string
	RuntimeEnvPath                      string
	ReleaseEnvPath                      string
	ComposeFile                         string
	AllowedTenantID                     string
	ComposeProject                      string
	DockerBinary                        string
	Timeout                             time.Duration
	Profile                             productionSubsystemProfile
	RuntimeBootstrapFiles               []productionSubsystemRuntimeFileManifest
	AllowPlaceholderDatabaseCredentials bool
}

type productionComposeTarget struct {
	config productionComposeTargetConfig
	runner subsystemCommandRunner
	mutex  sync.Mutex
}

// NewProductionComposeSubsystemProvisioner 在 Agent 启动时一次加载审核清单；清单错误直接阻止
// 执行器启动，避免运行中才发现目标、模板或路径不受信任。
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
		dockerBinary: config.DockerBinary,
	}
	provisioner.capabilities.Enabled = config.Enabled
	for _, profile := range profiles {
		target := &productionComposeTarget{config: productionComposeTargetConfig{
			DeployRoot: root, RuntimeEnvPath: config.RuntimeEnvPath, ReleaseEnvPath: config.ReleaseEnvPath,
			ComposeFile: config.ComposeFile, AllowedTenantID: config.AllowedTenantID,
			ComposeProject: config.ComposeProject, DockerBinary: config.DockerBinary,
			Timeout: config.Timeout, Profile: profile, RuntimeBootstrapFiles: runtimeBootstrapFiles,
			AllowPlaceholderDatabaseCredentials: config.AllowPlaceholderDatabaseCredentials,
		}, runner: runner}
		key := productionSubsystemTargetKey(profile.Manifest.Application.Code, profile.Manifest.Application.Environment)
		provisioner.targets[key] = target
	}
	return provisioner, nil
}

// DiscoverSubsystemCandidates reads only opt-in Docker labels.  It does not relax the reviewed
// production deployment manifest: administrators can inspect candidates, while provisioning
// remains restricted to approved targets until a generic runtime profile is introduced.
func (provisioner *ProductionComposeSubsystemProvisioner) DiscoverSubsystemCandidates(ctx context.Context) ([]application.SubsystemDiscoveryCandidate, error) {
	if provisioner == nil || !provisioner.enabled {
		return nil, provisioningError("production subsystem deployment is disabled")
	}
	return discoverDockerLabelCandidates(ctx, provisioner.dockerBinary)
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

// DiscoverSubsystemServices reads service labels from the reviewed production target's Docker
// project. The target allow-list remains authoritative; the request cannot select arbitrary Docker
// objects or alter any server-side runtime file.
func (provisioner *ProductionComposeSubsystemProvisioner) DiscoverSubsystemServices(ctx context.Context, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	target, err := provisioner.target(applicationCode, environment)
	if err != nil {
		return nil, err
	}
	return discoverDockerLabelServices(ctx, target.config.DockerBinary, applicationCode, environment)
}

// Preflight 在创建不可恢复的 OAuth 明文前验证租户、目标、文件权限、不可变镜像和 Compose
// 配置；请求只能选择审核目标，不能把文件、镜像、服务或命令注入 Agent。
func (target *productionComposeTarget) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	if err := target.validateTenant(input.TenantID); err != nil {
		return err
	}
	if err := target.validatePreflightInput(input); err != nil {
		return err
	}
	// 预检只验证部署文件本身及清单声明的运行文件结构。基础设施密钥可能仍
	// 是发布包中的占位值；这不应阻止控制面先登记接入目标，真正写运行配置
	// 和启动服务前再由 Provision 严格校验。
	if err := target.validateDeploymentFiles(true, false); err != nil {
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

// Provision 串行写入清单允许的键，并在与 CI/CD 共用的文件锁内执行固定步骤；锁同时保护
// 运行时文件、数据库备份和 Compose 操作，使自动重试不会与发布流程交叉覆盖。
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
	return target.deployLocked(operationContext, productionProvisioningSecrets(input)...)
}

// Update 只重用已落盘配置，不尝试从数据库恢复 OAuth 明文或隐式轮换密钥。重试/更新时
// 仍会把清单中的固定值（values）和缺失的生成密钥写入 runtime 文件，使清单新增的固定
// 配置（如测试服务器的非 Secure Cookie 开关）无需重新 onboarding 即可生效；绑定项需要
// 一次性 OAuth 明文，保持只在首次 Provision 写入。
func (target *productionComposeTarget) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	if err := target.validateTenant(input.TenantID); err != nil {
		return err
	}
	if !target.matches(input.ApplicationCode, input.Environment) {
		return provisioningError("production subsystem deployment request is invalid")
	}
	if err := target.validateDeploymentFiles(false, true); err != nil {
		return err
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		return err
	}
	if err := target.validateRuntimeIntegrationConfiguration(); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, target.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(target.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)
	return target.deployLocked(operationContext, productionProvisioningSecrets(input)...)
}

// validateRuntimeIntegrationConfiguration checks feature flags whose credentials are
// intentionally preserved across Update/Retry. Update does not receive one-time OAuth
// secrets, so starting Compose with an old/incomplete runtime file would only produce a
// misleading container health-check failure. Fail before touching Docker and identify the
// exact keys an operator must restore from the secret store.
func (target *productionComposeTarget) validateRuntimeIntegrationConfiguration() error {
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		path := filepath.Join(target.config.DeployRoot, filepath.FromSlash(runtimeFile.Path))
		content, err := os.ReadFile(path)
		if err != nil {
			return provisioningError("read production subsystem runtime configuration")
		}
		values := parseEnvironmentValues(string(content))
		if strings.EqualFold(strings.TrimSpace(values["CONTRACT_VERIFICATION_ENABLED"]), "true") {
			for _, key := range []string{"CONTRACT_SUMMARY_URL", "CONTRACT_SUMMARY_TOKEN_URL", "CONTRACT_SUMMARY_CLIENT_ID", "CONTRACT_SUMMARY_CLIENT_SECRET"} {
				if strings.TrimSpace(values[key]) == "" || strings.HasPrefix(values[key], "PENDING_") {
					return provisioningError("production subsystem runtime configuration is incomplete: " + key + " is required when CONTRACT_VERIFICATION_ENABLED=true")
				}
			}
		}
	}
	return nil
}

// Teardown 仅停止清单列出的运行服务，保留数据库、备份、运行时秘密和发布指针；因此失败
// 后可安全重试停止，且不会把可恢复数据误当作回滚对象删除。
// 下线不连接数据库，因此不校验数据库凭据；即使 .env 仍是占位值也应允许停止服务。
func (target *productionComposeTarget) Teardown(ctx context.Context, tenantID string) error {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	if err := target.validateTenant(tenantID); err != nil {
		return err
	}
	if err := target.validateDeploymentFiles(false, false); err != nil {
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

func (target *productionComposeTarget) deployLocked(ctx context.Context, redactValues ...string) error {
	// 部署按依赖、备份、迁移、服务启动顺序推进；中途失败只返回脱敏错误并保留备份，
	// 不自动删除已写入的运行配置或数据库状态，后续重试/人工恢复可从同一现场继续判断。
	compose := target.config.Profile.Manifest.Compose
	if len(compose.DependencyServices) > 0 {
		// Dependency startup must be isolated from the shared platform services.
		// Without --no-deps, Compose can recreate platform-api and the Agent,
		// briefly turning every browser request into a 502 during a subsystem switch.
		arguments := []string{"up", "-d", "--wait", "--wait-timeout", "240", "--no-deps"}
		arguments = append(arguments, compose.DependencyServices...)
		target.stepLog("step=dependencies services=%v", compose.DependencyServices)
		// 依赖步骤加硬超时：compose --wait 异常时不能无限挂起拖垮接入与平台。
		stepContext, cancelStep := context.WithTimeout(ctx, 5*time.Minute)
		err := target.runCompose(stepContext, arguments...)
		cancelStep()
		if err != nil {
			return target.subsystemServiceFailure(ctx, "start production subsystem dependencies", compose.DependencyServices, redactValues)
		}
	}
	if compose.Database != nil {
		target.stepLog("step=backup database=%s", compose.Database.Name)
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
		target.stepLog("step=migrate service=%s", compose.MigrateService)
		// 迁移是 run --rm 一次性容器，失败后无法再取日志；因此在失败时直接捕获
		// 本次迁移输出，脱敏后附到错误里让页面显示真实原因。
		if output, err := target.runComposeOutput(ctx, "run", "--rm", "--no-deps", compose.MigrateService); err != nil {
			detail := sanitizeProvisioningLog(string(output), redactValues)
			if detail == "" {
				return provisioningError("migrate production subsystem database")
			}
			return provisioningError("migrate production subsystem database: " + detail)
		}
	}
	arguments := []string{"up", "-d", "--wait", "--wait-timeout", "240", "--force-recreate", "--no-deps"}
	arguments = append(arguments, compose.RuntimeServices...)
	target.stepLog("step=runtime services=%v", compose.RuntimeServices)
	// 运行步骤同样加硬超时，避免健康检查或镜像拉取异常时长期挂起。
	stepContext, cancelStep := context.WithTimeout(ctx, 5*time.Minute)
	err := target.runCompose(stepContext, arguments...)
	cancelStep()
	if err != nil {
		return target.subsystemServiceFailure(ctx, "start production subsystem services", compose.RuntimeServices, redactValues)
	}
	return nil
}

// stepLog 把固定部署步骤写入 Agent 标准错误（容器日志），用于定位长耗时或卡住的步骤。
func (target *productionComposeTarget) stepLog(format string, args ...any) {
	manifest := target.config.Profile.Manifest.Application
	fmt.Fprintf(os.Stderr, "[subsystem-provisioner] code=%s env=%s "+format+"\n", append([]any{manifest.Code, manifest.Environment}, args...)...)
}

// subsystemServiceFailure 在依赖或目标 API 启动失败时，用受限超时抓取受影响容器最近日志，
// 脱敏后附加到 provisioning 错误里。这样平台页面能直接看到“为什么没通过健康检查”，而不是
// 只返回通用提示；抓取日志本身失败时仍回退为原始步骤错误，不影响既有错误语义。
func (target *productionComposeTarget) subsystemServiceFailure(ctx context.Context, step string, services []string, redactValues []string) error {
	if len(services) == 0 {
		return provisioningError(step)
	}
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return provisioningError(step)
	}
	logContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	arguments, environment := target.composeCommand("logs", "--tail", "80", "--no-color")
	arguments = append(arguments, services...)
	output, runErr := outputRunner.RunOutput(logContext, target.config.DeployRoot, environment, target.config.DockerBinary, arguments...)
	if runErr != nil {
		return provisioningError(step)
	}
	detail := sanitizeProvisioningLog(string(output), redactValues)
	if detail == "" {
		return provisioningError(step)
	}
	return provisioningError(step + ": " + detail)
}

func (target *productionComposeTarget) runCompose(ctx context.Context, arguments ...string) error {
	fullArguments, runnerEnvironment := target.composeCommand(arguments...)
	return target.runner.Run(ctx, target.config.DeployRoot, runnerEnvironment, target.config.DockerBinary, fullArguments...)
}

// runComposeOutput 与 runCompose 等价，但尽量通过 RunOutput 捕获命令输出，供失败时
// 回显容器日志；不支持的 runner 回退为仅返回错误。
func (target *productionComposeTarget) runComposeOutput(ctx context.Context, arguments ...string) ([]byte, error) {
	fullArguments, runnerEnvironment := target.composeCommand(arguments...)
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return nil, target.runner.Run(ctx, target.config.DeployRoot, runnerEnvironment, target.config.DockerBinary, fullArguments...)
	}
	return outputRunner.RunOutput(ctx, target.config.DeployRoot, runnerEnvironment, target.config.DockerBinary, fullArguments...)
}

// composeCommand 构造与 runCompose 相同的 docker compose 前缀和运行环境，供失败日志抓取复用。
func (target *productionComposeTarget) composeCommand(arguments ...string) ([]string, []string) {
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
	return append(prefix, arguments...), runnerEnvironment
}

// writeRuntimeFixedValues writes manifest constants, generated keys and the non-secret public
// runtime bindings when the management page supplied a changed gateway address. Secret bindings
// remain untouched because Update/Retry never receives or rotates one-time OAuth credentials.
func (target *productionComposeTarget) writeRuntimeFixedValues(input application.SubsystemProvisioningInput) error {
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
		values := make(map[string]string, len(runtimeFile.Values)+len(generatedValues))
		for key, value := range runtimeFile.Values {
			values[key] = value
		}
		for key, value := range generatedValues {
			values[key] = value
		}
		if strings.TrimSpace(input.PublicURL) != "" {
			for key, source := range runtimeFile.Bindings {
				if !isPublicRuntimeBinding(source) {
					continue
				}
				value, resolveErr := resolveProductionBinding(input, source)
				if resolveErr != nil {
					return resolveErr
				}
				values[key] = value
			}
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

func isPublicRuntimeBinding(source string) bool {
	switch source {
	case "public_origin", "redirect_uri", "logged_out_url", "public_url", "public_url_no_trailing_slash",
		"path_prefix", "upstream_url", "cookie_secure", "issuer_security_center_url":
		return true
	default:
		return false
	}
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
	case "issuer":
		return issuer, nil
	case "public_origin":
		publicOrigin, publicOriginErr := publicOriginFromURL(input.PublicURL)
		if publicOriginErr != nil {
			return "", provisioningError("production subsystem public URL is invalid")
		}
		return publicOrigin, nil
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
		publicOrigin, publicOriginErr := publicOriginFromURL(input.PublicURL)
		if publicOriginErr != nil {
			return "", provisioningError("production subsystem public URL is invalid")
		}
		parsed := mustParseURL(publicOrigin)
		return booleanEnvironmentValue(parsed != nil && strings.EqualFold(parsed.Scheme, "https")), nil
	case "catalog_publisher_client_id":
		return input.CatalogPublisherClientID, nil
	case "catalog_publisher_client_secret":
		return input.CatalogPublisherClientSecret, nil
	case "issuer_security_center_url":
		return issuer + "/settings/security", nil
	case "authorization_context_url":
		return productionAuthorizationContextURL, nil
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

func (target *productionComposeTarget) validateDeploymentFiles(requireWritableEnvironment, validateInfrastructureSecrets bool) error {
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
	// 测试服务器可显式允许数据库占位凭据，让 Agent 先完成接入；生产默认严格校验。
	if validateInfrastructureSecrets && !target.config.AllowPlaceholderDatabaseCredentials {
		if err := validateProductionRequiredEnvironmentKeys(target.config.RuntimeEnvPath, target.config.Profile.Manifest.Runtime.RequiredInfrastructureKeys, "production subsystem database credentials are incomplete"); err != nil {
			return err
		}
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

// productionProvisioningSecrets 收集本次一次性交付的明文凭据，供容器日志脱敏使用；
// 任何凭据都不会写回日志或平台页面。
func productionProvisioningSecrets(input application.SubsystemProvisioningInput) []string {
	values := make([]string, 0, 2+len(input.ServiceCredentials))
	for _, value := range []string{input.ClientSecret, input.CatalogPublisherClientSecret} {
		if value != "" {
			values = append(values, value)
		}
	}
	for _, credential := range input.ServiceCredentials {
		if credential.PlaintextSecret != "" {
			values = append(values, credential.PlaintextSecret)
		}
	}
	return values
}

// sanitizeProvisioningLog 截断并从容器日志中移除一次性明文凭据，避免错误详情成为秘密泄露面。
func sanitizeProvisioningLog(output string, redactValues []string) string {
	const limit = 4 * 1024
	result := strings.TrimSpace(output)
	for _, value := range redactValues {
		if value != "" {
			result = strings.ReplaceAll(result, value, "[REDACTED]")
		}
	}
	result = strings.TrimSpace(result)
	if len(result) > limit {
		result = result[:limit] + "...(truncated)"
	}
	return result
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
	publicOrigin, publicURLValid := publicURLOrigin(input.PublicURL, app.PathPrefix)
	expectedPublicURL := publicOrigin + app.PathPrefix + "/"
	expectedRedirectURI := publicOrigin + app.PathPrefix + "/auth/callback"
	if mustParseURL(issuer) == nil || strings.TrimSpace(input.ApplicationID) == "" || strings.TrimSpace(input.ClientID) == "" ||
		strings.TrimSpace(input.ClientSecret) == "" || strings.TrimSpace(input.CatalogPublisherClientID) == "" ||
		strings.TrimSpace(input.CatalogPublisherClientSecret) == "" || input.PathPrefix != app.PathPrefix ||
		strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/") != app.UpstreamURL || !publicURLValid || input.PublicURL != expectedPublicURL || input.RedirectURI != expectedRedirectURI {
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
	return target.validateDeploymentFiles(true, true)
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
	_, publicBaseValid := publicBaseOrigin(input.PublicBaseURL)
	if mustParseURL(issuer) == nil || !target.matches(input.ApplicationCode, input.Environment) ||
		strings.ToLower(strings.TrimSpace(input.ClientType)) != app.ClientType ||
		!publicBaseValid ||
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
