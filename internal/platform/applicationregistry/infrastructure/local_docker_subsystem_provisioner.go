package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
)

var subsystemDirectoryCodePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// publicOriginFromURL 从已登记的完整公开 URL 提取浏览器 origin；运行时 origin 不能携带子系统
// 路径，否则 OIDC issuer、回调和网关拼接会产生双重路径。
func publicOriginFromURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid public URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// publicBaseOrigin 校验管理员输入的 origin；子系统路径必须独立来自审核目标，避免把任意路径
// 写入运行时配置或网关规则。
func publicBaseOrigin(value string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
}

// normalizeAccessPublicOrigin 校验并归一化 apply-access 的 PublicOrigin。空值表示恢复本机访问；
// 非空值必须通过 validEnvironmentValue（拒绝 \r\n\x00）且是不含 path/query/fragment/userinfo 的
// 纯 http(s) origin。安全（SEC-F1）：该值会以 "KEY=origin" 逐行写入 .env.lan 并被 Compose 注入
// 多个容器，任何换行或畸形 scheme 都会换行注入任意环境变量（如 DEV_AUTH_ENABLED、OIDC_ISSUER）。
func normalizeAccessPublicOrigin(value string) (string, error) {
	origin := strings.TrimSpace(value)
	if origin == "" {
		return "", nil
	}
	if !validEnvironmentValue(origin) {
		return "", provisioningError("public origin contains disallowed control characters")
	}
	normalized, ok := publicBaseOrigin(origin)
	if !ok {
		return "", provisioningError("public origin must be an http(s) origin without path, query or credentials")
	}
	return normalized, nil
}

// publicURLOrigin 要求已生成 URL 精确匹配审核目标路径，同时允许管理员提供实际 origin；查询串、
// 片段和用户信息一律拒绝，防止配置被解释成另一种地址。
func publicURLOrigin(value, pathPrefix string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	origin, ok := publicBaseOrigin(parsed.Scheme + "://" + parsed.Host)
	if !ok || parsed.Path != strings.TrimRight(strings.TrimSpace(pathPrefix), "/")+"/" {
		return "", false
	}
	return origin, true
}

const (
	integratedContractApplicationCode   = "contract_management"
	integratedCustomerApplicationCode   = "customer_and_opportunity"
	integratedPortalApplicationCode     = "customer_portal"
	integratedProjectApplicationCode    = "project_management"
	integratedSettlementApplicationCode = "settlement"
	// 该哈希是客户授权目录的部署契约：customer_and_opportunity 的目录测试在角色映射变化时
	// 会故意失败，强制本值与应用内嵌目录在同一评审发布中同步更新。
	integratedCustomerRoleConfigHash = "sha256:858b32508d50185dc3b1551683b3c64b98617cae7d3a8b6e1ed967cbd4fb7f93"
	integratedPortalRoleConfigHash   = "sha256:b773a9ab0a43370b7a2f0d454f32c21a6ea405d2c8837ebb60b7b6783a245eef"
)

// LocalDockerSubsystemProvisionerConfig 配置一键接入端点使用的受信本机 Docker 自动化。
// 各路径属于运维配置而非浏览器输入，接入请求永远不能直接提供这些路径。
type LocalDockerSubsystemProvisionerConfig struct {
	Enabled                 bool
	ProjectsRoot            string
	GatewayScriptPath       string
	GatewayIncludePath      string
	PlatformComposeProject  string
	PlatformComposeFile     string
	PlatformFrontendService string
	PlatformDockerNetwork   string
	DockerBinary            string
	Timeout                 time.Duration
	// CatalogSync 配置接入完成后的授权目录同步钩子。启用后，provisioner 会在子系统 Compose
	// 栈起来之后于 Agent 主机运行经过评审的 contract_management 目录同步脚本，使平台的授权
	// 目录反映子系统的角色与权限声明，且不需要子系统进程内做任何带内同步。
	CatalogSyncEnabled        bool
	CatalogSyncImage          string
	CatalogSyncMysqlContainer string
	CatalogSyncMysqlUser      string
	CatalogSyncMysqlPassword  string
	CatalogSyncMysqlDatabase  string
	CatalogSyncTargetAppCode  string
	// Keycloak 端点由运维提供给隔离部署 Agent，不能从浏览器请求推导。PublicURL 是浏览器可见
	// 的 Keycloak 基地址；InternalURL 是 Compose 网络内端点，供 Go OIDC 客户端完成发现、JWKS、
	// UserInfo 与令牌交换。
	KeycloakPublicURL   string
	KeycloakInternalURL string
	KeycloakRealm       string
}

type subsystemCommandRunner interface {
	Run(context.Context, string, []string, string, ...string) error
}

type execSubsystemCommandRunner struct{}

// Run 执行外部命令并丢弃合并输出，仅向调用方返回错误；失败输出由 RunOutput 统一截断后写入
// Agent 标准错误，避免敏感参数进入返回值。
func (execSubsystemCommandRunner) Run(ctx context.Context, directory string, environment []string, name string, arguments ...string) error {
	_, err := execSubsystemCommandRunner{}.RunOutput(ctx, directory, environment, name, arguments...)
	return err
}

// RunOutput 成功时返回独立标准输出，失败时保留两路输出供受限诊断。
// 参数：ctx控制执行期限，directory为目录提示，environment为进程环境，name及arguments为命令。
// 返回值：成功的标准输出或失败诊断；执行失败时同时返回错误。
func (execSubsystemCommandRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	// `directory` 只是提示值：command.Dir 只接受真实目录，因此调用方可能传入 unix socket 路径
	// （如 /var/run/docker.sock）用于语义说明。此时回退到 "/"，避免 exec.Command 的 chdir 探测
	// 在非目录上误报 ENOTDIR。
	if info, err := os.Stat(directory); err == nil && info.IsDir() {
		command.Dir = directory
	} else {
		command.Dir = string(filepath.Separator)
	}
	command.Env = environment
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		// Compose 的成功警告不能混入 JSON；失败时仍保留原有诊断能力。
		if len(output) > 0 && diagnostics.Len() > 0 {
			output = append(output, '\n')
		}
		output = append(output, diagnostics.Bytes()...)
		// 把失败命令输出的截断摘要写到标准错误，供运维诊断部署失败。不得原样返回命令参数或
		// 输出：两者都可能包含实现细节。OAuth secret 从不作为命令行参数传入，但该规则也为
		// 后续改动兜底。
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] %s %v failed: %v\noutput: %s\n",
			name, truncateArgs(arguments), err, truncateOutput(output))
	}
	return output, err
}

func truncateArgs(args []string) []string {
	const max = 3
	if len(args) <= max {
		return args
	}
	out := make([]string, max+1)
	copy(out, args[:max])
	out[max] = "..."
	return out
}

func truncateOutput(output []byte) string {
	const limit = 2 * 1024
	if len(output) <= limit {
		return string(output)
	}
	return string(output[:limit]) + "...(truncated)"
}

// LocalDockerSubsystemProvisioner 把 OIDC 配置写入受控的兄弟项目，启动 Compose，并更新平台
// 管理的 nginx include；它只负责运行时副作用，数据库登记和删除由应用层负责。
type LocalDockerSubsystemProvisioner struct {
	config LocalDockerSubsystemProvisionerConfig
	runner subsystemCommandRunner
	mutex  sync.Mutex
	// roleConfigHashes 缓存从本地子镜像内嵌 authz-catalog 读到的目录哈希，
	// 避免平台侧硬编码常量与业务仓库授权目录漂移。
	roleConfigHashes map[string]string
	// operationSecrets 保存本次受控操作交付的明文凭据，仅用于失败诊断脱敏；
	// 它在部署互斥内设置并清除，绝不写入日志、错误文本或返回值。
	operationSecrets []string
}

// NewLocalDockerSubsystemProvisioner 构造本机部署自动化适配器；配置不完整时返回错误，
// 不会启动任何容器或改写任何文件。
func NewLocalDockerSubsystemProvisioner(config LocalDockerSubsystemProvisionerConfig) (*LocalDockerSubsystemProvisioner, error) {
	return newLocalDockerSubsystemProvisioner(config, execSubsystemCommandRunner{})
}

func newLocalDockerSubsystemProvisioner(config LocalDockerSubsystemProvisionerConfig, runner subsystemCommandRunner) (*LocalDockerSubsystemProvisioner, error) {
	if runner == nil {
		return nil, errors.New("subsystem command runner is required")
	}
	config.ProjectsRoot = strings.TrimSpace(config.ProjectsRoot)
	config.GatewayScriptPath = strings.TrimSpace(config.GatewayScriptPath)
	config.GatewayIncludePath = strings.TrimSpace(config.GatewayIncludePath)
	config.PlatformComposeProject = strings.TrimSpace(config.PlatformComposeProject)
	config.PlatformFrontendService = strings.TrimSpace(config.PlatformFrontendService)
	config.PlatformDockerNetwork = strings.TrimSpace(config.PlatformDockerNetwork)
	config.DockerBinary = strings.TrimSpace(config.DockerBinary)
	config.KeycloakPublicURL = strings.TrimRight(strings.TrimSpace(config.KeycloakPublicURL), "/")
	config.KeycloakInternalURL = strings.TrimRight(strings.TrimSpace(config.KeycloakInternalURL), "/")
	config.KeycloakRealm = strings.Trim(strings.TrimSpace(config.KeycloakRealm), "/")
	if config.DockerBinary == "" {
		config.DockerBinary = "docker"
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Minute
	}
	return &LocalDockerSubsystemProvisioner{config: config, runner: runner, operationSecrets: localOperationSecrets(config)}, nil
}

// localOperationSecrets 收集本地 provisioner 配置里真实存在的凭据明文，供失败诊断输出脱敏
// （Compose 输出与容器日志都可能回显密码）。只收配置结构体里实际会下发给 Compose 的字段，
// 空值与重复值一律跳过；这些明文不得出现在错误摘要、日志或状态表里。
func localOperationSecrets(config LocalDockerSubsystemProvisionerConfig) []string {
	candidates := []string{config.CatalogSyncMysqlPassword}
	secrets := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		// 脱敏必须比对原文：裁剪空白会改变密码本身，导致输出里的真实值漏网。
		if candidate == "" || strings.TrimSpace(candidate) == "" || slices.Contains(secrets, candidate) {
			continue
		}
		secrets = append(secrets, candidate)
	}
	return secrets
}

// oidcBackchannelBaseURL 仅当所选 issuer 是本部署的 Keycloak Realm 时返回 Compose 私网端点。
// OIDC_ISSUER 仍保留公开 issuer，让 go-oidc 校验浏览器可见的 iss 声明，而其 HTTP 传输走私网
// 访问 Keycloak，避免绕行宿主网络。返回值只是 origin（发现路径由 OIDC 客户端自行拼接）；
// 其中不得包含 /realms/<realm>，这是 CRM、Portal 和 contract OIDC 配置校验器的硬性要求。
func (provisioner *LocalDockerSubsystemProvisioner) oidcBackchannelBaseURL(issuer string) string {
	if provisioner == nil || provisioner.config.KeycloakPublicURL == "" ||
		provisioner.config.KeycloakInternalURL == "" || provisioner.config.KeycloakRealm == "" {
		return ""
	}
	publicIssuer := provisioner.config.KeycloakPublicURL + "/realms/" + provisioner.config.KeycloakRealm
	if strings.TrimRight(strings.TrimSpace(issuer), "/") != publicIssuer {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(provisioner.config.KeycloakInternalURL), "/")
}

// DiscoverSubsystemServices 通过既有 Agent 传输报告 Compose 容器。发现是只读操作，
// 不改写 .env、Compose 或网关文件。
func (provisioner *LocalDockerSubsystemProvisioner) DiscoverSubsystemServices(ctx context.Context, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	if provisioner == nil || !provisioner.config.Enabled {
		return nil, provisioningError("automatic subsystem deployment is disabled")
	}
	return discoverDockerLabelServices(ctx, provisioner.config.DockerBinary, applicationCode, environment)
}

// DiscoverSubsystemCandidates 只返回 label 声明且尚未登记的服务元数据。结果不能写文件或
// 启动容器，因此可以安全暴露给经过审计的接入页面。
func (provisioner *LocalDockerSubsystemProvisioner) DiscoverSubsystemCandidates(ctx context.Context) ([]application.SubsystemDiscoveryCandidate, error) {
	if provisioner == nil || !provisioner.config.Enabled {
		return nil, provisioningError("automatic subsystem deployment is disabled")
	}
	return discoverDockerLabelCandidates(ctx, provisioner.config.DockerBinary)
}

// Preflight 在创建数据库聚合和一次性 OAuth 明文前拒绝缺失或不安全的本地项目配置，先完成
// 项目根目录、Compose、模板、网关和 Docker 可用性检查。
func (provisioner *LocalDockerSubsystemProvisioner) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	applicationCode := strings.TrimSpace(input.ApplicationCode)
	projectDirectory, err := provisioner.projectDirectory(applicationCode)
	if err != nil {
		return err
	}
	if isIntegratedSubsystem(applicationCode) {
		if _, _, _, _, _, _, _, _, err = provisioner.integratedComposeConfiguration(); err != nil {
			return err
		}
	} else if _, err = locateComposeFile(projectDirectory); err != nil {
		return provisioningError("subsystem Compose file is unavailable")
	}
	if _, _, err = provisioner.subsystemEnvironmentPaths(applicationCode, projectDirectory); err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	if info, statErr := os.Stat(provisioner.config.GatewayScriptPath); statErr != nil || info.IsDir() {
		return provisioningError("portal gateway script is unavailable")
	}
	if strings.TrimSpace(provisioner.config.GatewayIncludePath) == "" {
		return provisioningError("portal gateway include path is unavailable")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := provisioner.runner.Run(checkCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return provisioningError("Docker service is unavailable")
	}
	return nil
}

// Provision 用途：在部署互斥内执行本地受控部署并持久化成功回执。
// 参数：ctx 为上下文，input 为已授权目标与部署参数。
// 返回值：执行或回执保存失败时返回错误。
// needsRoleConfigHashes 只对会写入角色目录哈希的应用读取镜像目录，
// 避免无关部署（如合同）产生额外只读容器调用。
func needsRoleConfigHashes(applicationCode string) bool {
	return applicationCode == integratedCustomerApplicationCode || applicationCode == integratedPortalApplicationCode
}

// localRoleConfigHash 返回授权目录声明哈希：优先使用本次部署从子镜像内嵌
// authz-catalog 读取的值，读不到时回退到已审核常量，保持与旧镜像兼容。
func (provisioner *LocalDockerSubsystemProvisioner) localRoleConfigHash(application string) string {
	if hash := strings.TrimSpace(provisioner.roleConfigHashes[application]); hash != "" {
		return hash
	}
	if application == "portal" {
		return integratedPortalRoleConfigHash
	}
	return integratedCustomerRoleConfigHash
}

// refreshRoleConfigHashes 用一次性只读容器读取子镜像内嵌的授权目录哈希。
// 容器无网络、无挂载，仅执行 authz-catalog print；任何失败都保留常量回退，
// 不阻断部署，也不写入猜测值。
func (provisioner *LocalDockerSubsystemProvisioner) refreshRoleConfigHashes(ctx context.Context) {
	images, err := provisioner.localComposeImages(ctx)
	if err != nil {
		return
	}
	runner, ok := provisioner.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return
	}
	hashes := map[string]string{}
	for application, service := range map[string]string{"crm": "customer-api", "portal": "portal-api"} {
		image := strings.TrimSpace(images[service])
		if image == "" {
			continue
		}
		out, runErr := runner.RunOutput(ctx, provisioner.localPlatformRoot(), os.Environ(), provisioner.config.DockerBinary,
			"run", "--rm", "--network", "none", "--entrypoint", "/app/authz-catalog", image, "print", application)
		if runErr != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "claims_role_config_hash=") {
				hashes[application] = strings.TrimSpace(strings.TrimPrefix(line, "claims_role_config_hash="))
			}
		}
	}
	provisioner.roleConfigHashes = hashes
}

func (provisioner *LocalDockerSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	// 所有接入/更新/下线共享同一把进程锁，因为它们会改写公共 Compose 与 nginx 配置；
	// 并发执行可能让一个请求覆盖另一个请求刚写好的密钥或网关片段。
	if err := validateAccessReceipt(input); err != nil {
		return err
	}
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.operationSecrets = deploymentDiagnosticSecrets(input)
	defer func() { provisioner.operationSecrets = nil }()
	if needsRoleConfigHashes(input.ApplicationCode) {
		provisioner.refreshRoleConfigHashes(ctx)
	}
	if err := provisioner.applyLocked(ctx, input); err != nil {
		return err
	}
	return writeAccessDeploymentReceipt(provisioner.localPlatformRoot(), input)
}

// Update 用途：在部署互斥内执行本地受控部署并持久化成功回执。
// 参数：ctx 为上下文，input 为已授权目标与部署参数。
// 返回值：执行或回执保存失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if err := validateAccessReceipt(input); err != nil {
		return err
	}
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.operationSecrets = deploymentDiagnosticSecrets(input)
	defer func() { provisioner.operationSecrets = nil }()
	if needsRoleConfigHashes(input.ApplicationCode) {
		provisioner.refreshRoleConfigHashes(ctx)
	}
	if err := provisioner.rebuildLocked(ctx, input); err != nil {
		return err
	}
	return writeAccessDeploymentReceipt(provisioner.localPlatformRoot(), input)
}

// Teardown 停止 Compose、删除生成的 .env.local、移除门户网关片段并 reload nginx；它不删除
// 控制面记录，HTTP 层随后负责 DELETE，因此清理失败可单独重试，不会误报数据库已回滚。
func (provisioner *LocalDockerSubsystemProvisioner) Teardown(ctx context.Context, _ /* 租户 */ string, applicationCode, environment string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()

	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	applicationCode = strings.TrimSpace(applicationCode)
	if !subsystemDirectoryCodePattern.MatchString(applicationCode) {
		return provisioningError("subsystem project path is invalid")
	}

	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	// 集成子系统与平台共用 Compose 项目，因此只能停止自己的运行组件，不能执行 down；
	// 独立子系统则拥有完整栈和 .env.local，可按项目整体清理。
	// 独立子系统的 Compose 栈和 .env.local 位于其项目目录内；集成的 contract、customer 和
	// Portal API 与平台共用 Compose 项目，因此 teardown 只停止其运行组件，并有意保留共享
	// 运行环境文件中的数据库/密钥材料。
	projectDirectory, projectErr := provisioner.projectDirectory(applicationCode)
	if projectErr == nil {
		localApproval := filepath.Join(provisioner.localPlatformRoot(), "runtime-approvals", "local-"+applicationCode+"-dev.json")
		_, localApprovalErr := os.Lstat(localApproval)
		if isIntegratedSubsystem(applicationCode) || (localApprovalErr == nil && len(localLicenseServices[applicationCode]) > 0) {
			services := []string{"contract-api", "contract-worker"}
			if applicationCode == "reimbursement" {
				services = []string{"reimbursement-api", "reimbursement-worker"}
			}
			if applicationCode == integratedCustomerApplicationCode {
				services = []string{"customer-api"}
			} else if applicationCode == integratedPortalApplicationCode {
				services = []string{"portal-api", "portal-report-worker"}
			}
			if applicationCode == integratedProjectApplicationCode {
				services = []string{"project-api", "sla-notifier"}
			}
			if localApprovalErr == nil && len(localLicenseServices[applicationCode]) > 0 {
				services = append([]string(nil), localLicenseServices[applicationCode]...)
			}
			var background *localProjectBackground
			if applicationCode == integratedProjectApplicationCode {
				var err error
				background, err = provisioner.projectBackgroundTarget(operationCtx)
				if err != nil {
					return err
				}
				if err := prepareProjectBackground(operationCtx, background, true); err != nil {
					return err
				}
			}
			if runErr := provisioner.runIntegratedPlatformCompose(operationCtx, append([]string{"stop"}, services...)...); runErr != nil {
				return provisioningError("stop subsystem containers")
			}
			if background != nil {
				if err := markProjectBackgroundStopped(background); err != nil {
					return err
				}
			}
		} else {
			if composeFile, composeErr := locateComposeFile(projectDirectory); composeErr == nil {
				runErr := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
					"compose", "--project-directory", projectDirectory, "-f", composeFile, "down", "--remove-orphans")
				if runErr != nil {
					return provisioningError("stop subsystem containers")
				}
			}
			environmentPath := filepath.Join(projectDirectory, ".env.local")
			if removeErr := os.Remove(environmentPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return provisioningError("remove subsystem environment file")
			}
		}
	}

	if !isIntegratedSubsystem(applicationCode) {
		gatewayEnvironment := append(os.Environ(), "PORTAL_GATEWAY_NGINX_INCLUDE="+provisioner.config.GatewayIncludePath)
		if gatewayErr := provisioner.runner.Run(operationCtx, filepath.Dir(provisioner.config.GatewayScriptPath), gatewayEnvironment,
			"/bin/bash", provisioner.config.GatewayScriptPath, "remove", applicationCode); gatewayErr != nil {
			return provisioningError("remove portal gateway entry")
		}
	}

	// 此处 nginx reload 必须成功：网关 include 条目已被移除，reload 失败会让已下线子系统仍然
	// 可路由。前端栈缺失仍被容忍，因为此时没有运行中的网关可刷新。
	if projectDirectory != "" && !isIntegratedSubsystem(applicationCode) {
		if containerID, err := provisioner.frontendContainerID(operationCtx, projectDirectory); err == nil {
			if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-t"); err != nil {
				return provisioningError("validate portal gateway configuration")
			}
			if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-s", "reload"); err != nil {
				return provisioningError("reload portal gateway")
			}
		}
	}
	return nil
}

// rebuildLocked 重建子系统编排并保留已选独立来源及已有一次性凭据。
// 参数：ctx为操作上下文，input为已授权的目标及公开运行配置。
// 返回值：配置、许可或重建失败时返回错误，不清空旧业务队列。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	// 许可运行态前置判定必须在任何文件重写与容器操作之前完成：目标运行配置要求商业许可
	// 运行态而本次没有凭据时，放行只会得到一个启动即失败、每分钟重启的容器。
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	if err := provisioner.requireLocalLicenseRuntimeState(input); err != nil {
		return err
	}
	if err := provisioner.deliverLocalRuntimeLicense(ctx, input); err != nil {
		return err
	}

	// 许可 fail-closed 前置判定已在上方自动合并保留（同一检查，不重复执行）。
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()
	// Settlement 编译进统一的平台 Compose 拓扑，绝不能落入独立项目分支——那会在同一浏览器
	// 路由下创建第二套数据库/网络。
	if input.ApplicationCode == integratedSettlementApplicationCode {
		directory, err := provisioner.projectDirectory(integratedSettlementApplicationCode)
		if err != nil {
			return err
		}
		path := filepath.Join(directory, ".env.local")
		values := map[string]string{}
		if err := provisioner.p1LocalRuntimeValues(input, path, values, false); err != nil {
			return err
		}
		if err := preserveLocalBusinessIntegrationFlags(input.ApplicationCode, path, path, values); err != nil {
			return provisioningError("read Settlement business source selection")
		}
		if err := updateSubsystemEnvironment(path, path, values); err != nil {
			return provisioningError("write Settlement source binding")
		}
		if err := provisioner.updateSettlementCatalogRuntimeConfiguration(input); err != nil {
			return err
		}
		var deploymentErr error
		if len(input.RuntimeLicenseCredentials) > 0 {
			deploymentErr = provisioner.deployLicensedLocalStack(operationCtx, input)
		} else {
			deploymentErr = provisioner.rebuildIntegratedSettlementStack(operationCtx, input)
		}
		if err := deploymentErr; err != nil {
			return localRuntimeDeploymentError(err, "rebuild settlement containers")
		}
		return nil
	}

	projectDirectory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return err
	}
	_, environmentPath, err := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		if err := provisioner.validateRetainedContractRuntime(operationCtx, environmentPath); err != nil {
			return err
		}
	}
	if err := provisioner.updateOIDCRuntimeConfiguration(input, environmentPath); err != nil {
		return err
	}
	if err := provisioner.updateServiceCredentialRuntimeConfiguration(input, environmentPath); err != nil {
		return err
	}
	if strings.TrimSpace(input.PublicURL) != "" {
		if err := provisioner.updatePublicRuntimeConfiguration(input, environmentPath); err != nil {
			return err
		}
	}
	var rebuildErr error
	switch {
	case len(input.RuntimeLicenseCredentials) > 0:
		rebuildErr = provisioner.deployLicensedLocalStack(operationCtx, input)
	case input.ApplicationCode == integratedContractApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case input.ApplicationCode == integratedCustomerApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case input.ApplicationCode == integratedPortalApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	case input.ApplicationCode == "reimbursement":
		rebuildErr = provisioner.rebuildIntegratedReimbursementStack(operationCtx)
	case input.ApplicationCode == integratedProjectApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedProjectStack(operationCtx)
	default:
		composeFile, locateErr := locateComposeFile(projectDirectory)
		if locateErr != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
		rebuildErr = provisioner.runStandaloneCompose(operationCtx, projectDirectory, environmentPath, composeFile,
			"up", "-d", "--build")
	}
	if rebuildErr != nil {
		return localRuntimeDeploymentError(rebuildErr, "rebuild subsystem containers")
	}
	// 受控重建后重新发布授权目录，使子系统重启期间发生的角色/权限集合变化反映到平台侧。
	// 同步是尽力而为：失败只记日志，不会中止更新响应。
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] post-rebuild catalog sync skipped or failed: %v\n", err)
	}
	return nil
}

// updateServiceCredentialRuntimeConfiguration 更新受控机器凭据并保留明确独立来源的关闭选择。
// 参数：input为已登记目标和凭据，environmentPath为运维控制的现有运行文件。
// 返回值：身份、来源读取或原子文件写入失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) updateServiceCredentialRuntimeConfiguration(input application.SubsystemProvisioningInput, environmentPath string) error {
	values := map[string]string{}
	switch input.ApplicationCode {
	case integratedCustomerApplicationCode:
		auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		if !ok {
			return provisioningError("customer audit publisher credential is unavailable")
		}
		expectedAuditClientID := input.ApplicationCode + "-" + input.Environment + "-audit-publisher"
		if auditCredential.OAuthClient.ClientID != expectedAuditClientID {
			return provisioningError("customer audit publisher credential does not match the target environment")
		}
		notificationCredential, ok := input.ServiceCredential(application.ServiceCredentialNotificationIngest)
		if !ok {
			return provisioningError("customer notification publisher credential is unavailable")
		}
		expectedNotificationClientID := input.ApplicationCode + "-" + input.Environment + "-notification-publisher"
		if notificationCredential.OAuthClient.ClientID != expectedNotificationClientID {
			return provisioningError("customer notification publisher credential does not match the target environment")
		}
		values["PLATFORM_APPLICATION_CODE"] = input.ApplicationCode
		values["PLATFORM_ENVIRONMENT_CODE"] = input.Environment
		values["PLATFORM_AUDIT_CLIENT_ID"] = auditCredential.OAuthClient.ClientID
		values["PLATFORM_AUDIT_CLIENT_SECRET"] = auditCredential.PlaintextSecret
		values["PLATFORM_NOTIFICATION_CLIENT_ID"] = notificationCredential.OAuthClient.ClientID
		values["PLATFORM_NOTIFICATION_CLIENT_SECRET"] = notificationCredential.PlaintextSecret
		ownerCredential, ok := input.ServiceCredential(application.ServiceCredentialOwnerDirectoryRead)
		if !ok {
			return provisioningError("customer owner directory credential is unavailable")
		}
		expectedOwnerClientID := input.ApplicationCode + "-" + input.Environment + "-owner-directory"
		if ownerCredential.OAuthClient.ClientID != expectedOwnerClientID {
			return provisioningError("customer owner directory credential does not match the target environment")
		}
		values["OWNER_DIRECTORY_ENABLED"] = "true"
		values["PLATFORM_OWNER_DIRECTORY_URL"] = "http://platform-api:8080/api/v1/internal/owner-directory"
		values["PLATFORM_MANAGEMENT_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["PLATFORM_OWNER_DIRECTORY_SCOPE"] = "owner_directory.read"
		values["PLATFORM_OWNER_DIRECTORY_CLIENT_ID"] = ownerCredential.OAuthClient.ClientID
		values["PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"] = ownerCredential.PlaintextSecret
		signedCountCredential, ok := input.ServiceCredential(application.ServiceCredentialContractOpportunitySignedCountRead)
		if !ok {
			return provisioningError("customer contract signed count credential is unavailable")
		}
		expectedSignedCountClientID := input.ApplicationCode + "-" + input.Environment + "-contract-signed-count"
		if signedCountCredential.OAuthClient.ClientID != expectedSignedCountClientID {
			return provisioningError("customer contract signed count credential does not match the target environment")
		}
		values["CONTRACT_SIGNED_COUNT_ENABLED"] = "true"
		values["CONTRACT_SIGNED_COUNT_URL"] = "http://contract-api:8081/contract_management/internal/opportunity-contract-counts/query"
		values["CONTRACT_SIGNED_COUNT_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["CONTRACT_SIGNED_COUNT_CLIENT_ID"] = signedCountCredential.OAuthClient.ClientID
		values["CONTRACT_SIGNED_COUNT_CLIENT_SECRET"] = signedCountCredential.PlaintextSecret
		values["CONTRACT_SIGNED_COUNT_SCOPE"] = "contract.opportunity_signed_count.read"
		// 合同摘要核验与商机签约事件上报的机器身份属于客户应用：合同侧验签比对
		// claims.Subject 与 customer_and_opportunity-<env>-contract-summary/-opportunity-intake，
		// 只有客户自己的这两个凭据能通过。缺失或串环境时失败关闭，不用合同侧凭据兜底。
		if err := customerContractIdentityRuntimeValues(input, values); err != nil {
			return err
		}
		// 附件写入文件网关：随受控更新与凭据原子交付。历史环境从未写入这些键，
		// CRM 附件一直停在 legacy 模式（能力关闭）。
		if credential, ok := input.ServiceCredential(application.ServiceCredentialFileGatewayWrite); ok &&
			strings.TrimSpace(credential.OAuthClient.ClientID) != "" && strings.TrimSpace(credential.PlaintextSecret) != "" {
			values["ATTACHMENT_FILE_GATEWAY_MODE"] = "dual"
			values["ATTACHMENT_FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
			values["ATTACHMENT_FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["ATTACHMENT_FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
			values["ATTACHMENT_FILE_GATEWAY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["ATTACHMENT_FILE_GATEWAY_CLIENT_SECRET"] = credential.PlaintextSecret
		}
	case integratedProjectApplicationCode:
		// 受控更新会轮换项目审计发布者密钥；更新路径必须把新 client/secret 写回运行文件，
		// 否则项目继续用旧 secret 调用平台审计入口并持续 401。首次接入由 applyLocked 下发，
		// 这里补齐更新路径，保持两条路径一致。
		auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		if !ok {
			return provisioningError("project audit publisher credential is unavailable")
		}
		expectedAuditClientID := input.ApplicationCode + "-" + input.Environment + "-audit-publisher"
		if auditCredential.OAuthClient.ClientID != expectedAuditClientID {
			return provisioningError("project audit publisher credential does not match the target environment")
		}
		values["PLATFORM_AUDIT_CLIENT_ID"] = auditCredential.OAuthClient.ClientID
		values["PLATFORM_AUDIT_CLIENT_SECRET"] = auditCredential.PlaintextSecret
	case integratedContractApplicationCode:
		if err := contractNotificationRuntimeValues(input, values, false); err != nil {
			return err
		}
		// 常规更新保留既有 owner-directory 凭据；重试（RETRY）会在上次投递可能失败时补充新
		// secret。CRM 把机器路由挂在自己的应用前缀下，普通更新即修复这一托管地址，不轮换
		// 现有凭据。
		values["CRM_REFERENCE_BASE_URL"] = "http://customer-api:8090/customer-opportunity"
		if credential, ok := input.ServiceCredential(application.ServiceCredentialOwnerDirectoryRead); ok {
			values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"] = credential.PlaintextSecret
		}
		if credential, ok := input.ServiceCredential(application.ServiceCredentialCRMContractReferenceRead); ok {
			values["CRM_REFERENCE_ENABLED"] = "true"
			values["CRM_REFERENCE_BASE_URL"] = "http://customer-api:8090/customer-opportunity"
			values["CRM_REFERENCE_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["CRM_REFERENCE_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["CRM_REFERENCE_CLIENT_SECRET"] = credential.PlaintextSecret
			values["CRM_REFERENCE_SCOPE"] = "customer.contract_reference.read"
		}
		if credential, ok := input.ServiceCredential(application.ServiceCredentialProjectContractImport); ok {
			values["PROJECT_INTEGRATION_ENABLED"] = "true"
			values["PROJECT_API_BASE_URL"] = "http://project-api:8082"
			values["PROJECT_INTEGRATION_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["PROJECT_INTEGRATION_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["PROJECT_INTEGRATION_CLIENT_SECRET"] = credential.PlaintextSecret
			values["PROJECT_INTEGRATION_AUDIENCE"] = "basic-platform-application"
		}
		if credential, ok := input.ServiceCredential(application.ServiceCredentialFileGatewayWrite); ok {
			// 外部合同创建与批量导入把原件写入文件网关；mode 不可为 legacy，
			// 且 application_id 必须与绑定归属一致，随受控更新与凭据同原子交付。
			values["STAMPED_FILE_GATEWAY_MODE"] = "dual"
			values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
			values["FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
			values["FILE_GATEWAY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["FILE_GATEWAY_CLIENT_SECRET"] = credential.PlaintextSecret
			values["FILE_GATEWAY_SCOPE"] = "platform:file:upload platform:file:bind platform:file:download"
		}
		// 本地/开发栈与生产清单使用同一组合同侧机器入口：CRM 商机签约事件（intake）
		// 与合同摘要的调用方固定为 customer_and_opportunity，client id 按
		// 「应用-环境-用途后缀」随 input.Environment 推导；这里只下发公开的信任域
		// 参数，不写死 prod 值、也不引入任何密钥。缺少这些键时合同进程会关闭对应
		// 端点，本地无法复现 P0（商机确认签约）与 CRM 合同摘要核验。
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_ENABLED"] = "true"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRE_BEARER"] = "true"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_ISSUER"] = "basic-platform"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_AUDIENCE"] = "basic-platform-application"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CLIENT_ID"] = integratedCustomerApplicationCode + "-" + input.Environment + "-opportunity-intake"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_APPLICATION_CODE"] = integratedCustomerApplicationCode
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE"] = "opportunity.signed.write"
		values["CONTRACT_SUMMARY_MACHINE_ENABLED"] = "true"
		values["CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER"] = "true"
		values["CONTRACT_SUMMARY_MACHINE_ISSUER"] = "basic-platform"
		values["CONTRACT_SUMMARY_MACHINE_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CONTRACT_SUMMARY_MACHINE_CLIENT_ID"] = integratedCustomerApplicationCode + "-" + input.Environment + "-contract-summary"
		values["CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE"] = integratedCustomerApplicationCode
		values["CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE"] = "contract.summary.read"
		// 合同→CRM 回写默认关闭：本地没有受控的 crm_contract_link_write 凭据时把
		// 地址留空，合同进程据此停用 dispatcher，而不是拿着占位 id/secret 在启动
		// 校验阶段失败关闭（crash-loop）。受控更新交付完整凭据后才打开回写。
		values["CRM_LINK_BASE_URL"] = ""
		if credential, ok := input.ServiceCredential(application.ServiceCredentialCRMContractLinkWrite); ok &&
			strings.TrimSpace(credential.OAuthClient.ClientID) != "" && strings.TrimSpace(credential.PlaintextSecret) != "" {
			values["CRM_LINK_BASE_URL"] = "http://customer-api:8090/customer-opportunity"
			values["CRM_LINK_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["CRM_LINK_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["CRM_LINK_CLIENT_SECRET"] = credential.PlaintextSecret
			values["CRM_LINK_SCOPE"] = "opportunity.signed.write"
		}
	}
	if err := provisioner.p1LocalRuntimeValues(input, environmentPath, values, false); err != nil {
		return err
	}
	// 门户的机器凭据原先只在首次接入（applyLocked）写入；受控更新/收编路径
	// 不写就会一直沿用旧 Secret，运行起来直接 401。这里补齐，并逐键核对
	// 写入值确实来自本次交付。
	if input.ApplicationCode == integratedPortalApplicationCode {
		if err := provisioner.portalServiceCredentialValues(input, environmentPath, values); err != nil {
			return err
		}
	}
	if len(values) == 0 {
		return nil
	}
	if err := preserveLocalBusinessIntegrationFlags(input.ApplicationCode, environmentPath, environmentPath, values); err != nil {
		return provisioningError("read subsystem business source selection")
	}
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem service credential configuration")
	}
	return nil
}

// customerContractIdentityRuntimeValues 下发客户应用侧「合同摘要核验」与「商机签约事件」
// 的客户端身份。合同 API 的机器验签严格要求 claims.Subject 等于
// customer_and_opportunity-<env>-contract-summary / -opportunity-intake，因此这两组凭据
// 必须来自客户应用自己的 ServiceCredential，不能用合同应用的同类用途凭据替代。
// 参数：input 为已授权的目标子系统与凭据，values 为待写入客户运行文件的键值。
// 返回值：凭据缺失或 client id 不属于目标环境时返回 provisioningError。
// portalServiceCredentialValues 把本次受控交付的门户机器凭据写入运行配置。
// 首次接入与受控更新共用同一入口，避免更新路径沿用历史 Secret 导致运行时 401。
func (provisioner *LocalDockerSubsystemProvisioner) portalServiceCredentialValues(input application.SubsystemProvisioningInput, environmentPath string, values map[string]string) error {
	localPortal, err := localPortalModeFromFile(environmentPath)
	if err != nil {
		return err
	}
	credentials, err := portalRuntimeCredentials(input, localPortal)
	if err != nil {
		return err
	}
	audit := credentials[application.ServiceCredentialAuditIngest]
	invite := credentials[application.ServiceCredentialPortalInviteVerify]
	values["PLATFORM_AUDIT_CLIENT_ID"] = audit.OAuthClient.ClientID
	values["PLATFORM_AUDIT_CLIENT_SECRET"] = audit.PlaintextSecret
	values["PORTAL_CRM_INVITE_CLIENT_ID"] = invite.OAuthClient.ClientID
	values["PORTAL_CRM_INVITE_CLIENT_SECRET"] = invite.PlaintextSecret
	// 门户报告导出/附件走平台文件网关，读取 FILE_GATEWAY_*。该组键同样只在
	// 首次接入（local 模式）写入过，更新路径不刷就会保留历史 Secret 而 401。
	if fileCredential, ok := input.ServiceCredential(application.ServiceCredentialFileGatewayWrite); ok &&
		strings.TrimSpace(fileCredential.OAuthClient.ClientID) != "" && strings.TrimSpace(fileCredential.PlaintextSecret) != "" {
		values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
		values["FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
		values["FILE_GATEWAY_CLIENT_ID"] = fileCredential.OAuthClient.ClientID
		values["FILE_GATEWAY_CLIENT_SECRET"] = fileCredential.PlaintextSecret
		values["FILE_GATEWAY_SCOPE"] = "platform:file:upload platform:file:bind platform:file:download"
	}
	return verifyPortalDeliveredCredentials(input, values)
}

func customerContractIdentityRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	summaryCredential, ok := input.ServiceCredential(application.ServiceCredentialContractSummaryRead)
	if !ok {
		return provisioningError("customer contract summary credential is unavailable")
	}
	expectedSummaryClientID := input.ApplicationCode + "-" + input.Environment + "-contract-summary"
	if summaryCredential.OAuthClient.ClientID != expectedSummaryClientID {
		return provisioningError("customer contract summary credential does not match the target environment")
	}
	intakeCredential, ok := input.ServiceCredential(application.ServiceCredentialContractOpportunitySignedWrite)
	if !ok {
		return provisioningError("customer contract opportunity signed write credential is unavailable")
	}
	expectedIntakeClientID := input.ApplicationCode + "-" + input.Environment + "-opportunity-intake"
	if intakeCredential.OAuthClient.ClientID != expectedIntakeClientID {
		return provisioningError("customer contract opportunity signed write credential does not match the target environment")
	}
	values["CONTRACT_VERIFICATION_ENABLED"] = "true"
	values["CONTRACT_SUMMARY_URL"] = "http://contract-api:8081/internal/contract-summary"
	values["CONTRACT_SUMMARY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["CONTRACT_SUMMARY_CLIENT_ID"] = summaryCredential.OAuthClient.ClientID
	values["CONTRACT_SUMMARY_CLIENT_SECRET"] = summaryCredential.PlaintextSecret
	values["CONTRACT_SUMMARY_SCOPE"] = "contract.summary.read"
	values["CONTRACT_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["CONTRACT_OPPORTUNITY_INTAKE_URL"] = "http://contract-api:8081/internal/opportunity-signed-events"
	values["CONTRACT_CLIENT_ID"] = intakeCredential.OAuthClient.ClientID
	values["CONTRACT_CLIENT_SECRET"] = intakeCredential.PlaintextSecret
	values["CONTRACT_SCOPE"] = "opportunity.signed.write"
	return nil
}

// updatePublicRuntimeConfiguration 更新公开访问配置及已审核业务入口身份。
// 参数：input为目标与公开地址，environmentPath为运维控制的运行文件。
// 返回值：地址、身份或文件更新失败时返回错误，独立来源关闭决定保持有效。
func (provisioner *LocalDockerSubsystemProvisioner) updatePublicRuntimeConfiguration(input application.SubsystemProvisioningInput, environmentPath string) error {
	publicOrigin, err := publicOriginFromURL(input.PublicURL)
	if err != nil {
		return provisioningError("subsystem public URL is invalid")
	}
	secure := booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(publicOrigin), "https://"))
	localPortal := false
	if input.ApplicationCode == integratedPortalApplicationCode {
		localPortal, err = localPortalModeFromFile(environmentPath)
		if err != nil {
			return err
		}
	}
	values := map[string]string{}
	switch input.ApplicationCode {
	case integratedContractApplicationCode:
		values = map[string]string{
			"APP_PUBLIC_URL":                input.PublicURL,
			"APP_PATH_PREFIX":               input.PathPrefix,
			"OIDC_REDIRECT_URI":             input.RedirectURI,
			"OIDC_POST_LOGOUT_REDIRECT_URI": input.PublicURL,
			"OIDC_SESSION_COOKIE_SECURE":    secure,
		}
	case integratedCustomerApplicationCode:
		values = map[string]string{
			"APP_PUBLIC_ORIGIN":             publicOrigin,
			"APP_PATH_PREFIX":               input.PathPrefix,
			"OIDC_REDIRECT_URI":             input.RedirectURI,
			"OIDC_POST_LOGOUT_REDIRECT_URI": input.PublicURL,
			"OIDC_SESSION_COOKIE_SECURE":    secure,
		}
	case integratedPortalApplicationCode:
		values = map[string]string{
			"PORTAL_PUBLIC_ORIGIN":         publicOrigin,
			"PORTAL_PATH_PREFIX":           input.PathPrefix,
			"PORTAL_OIDC_REDIRECT_URI":     input.RedirectURI,
			"PORTAL_SESSION_COOKIE_SECURE": secure,
		}
	case integratedProjectApplicationCode:
		values = map[string]string{
			"APP_PATH_PREFIX":                              input.PathPrefix,
			"OIDC_REDIRECT_URI":                            input.RedirectURI,
			"OIDC_POST_LOGOUT_REDIRECT_URI":                input.PublicURL,
			"OIDC_SESSION_COOKIE_SECURE":                   secure,
			"CONTRACT_INTEGRATION_ENABLED":                 "true",
			"CONTRACT_INTEGRATION_REQUIRE_BEARER":          "true",
			"CONTRACT_INTEGRATION_CLIENT_ID":               integratedContractApplicationCode + "-" + input.Environment + "-project-integration",
			"CONTRACT_INTEGRATION_AUDIENCE":                "basic-platform-application",
			"CONTRACT_INTEGRATION_ISSUER":                  "basic-platform",
			"CONTRACT_INTEGRATION_PUBLIC_KEY_PATH":         "/app/data/keys/jwt-ed25519-public.pem",
			"CONTRACT_INTEGRATION_CALLER_APPLICATION_CODE": "contract_management",
			"CONTRACT_INTEGRATION_CALLER_ENVIRONMENT_CODE": input.Environment,
			"CONTRACT_INTEGRATION_REQUIRED_SCOPE":          "project.contract.import",
		}
		if err := projectNotificationRuntimeValues(input, values); err != nil {
			return err
		}
		// 服务项操作台从基础平台负责人目录选择团队负责人、项目经理和工程师。
		// 老环境尚未下发该凭据时保持缺省（前端会提示目录不可用），不阻断接入本身。
		if ownerCredential, ok := input.ServiceCredential(application.ServiceCredentialOwnerDirectoryRead); ok {
			values["OWNER_DIRECTORY_ENABLED"] = "true"
			values["PLATFORM_OWNER_DIRECTORY_URL"] = "http://platform-api:8080/api/v1/internal/owner-directory"
			values["PLATFORM_OWNER_DIRECTORY_SCOPE"] = "owner_directory.read"
			values["PLATFORM_OWNER_DIRECTORY_CLIENT_ID"] = ownerCredential.OAuthClient.ClientID
			values["PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"] = ownerCredential.PlaintextSecret
		}
		// 证据/报告与三类 CSV 的核验都依赖统一文件网关客户端：只有拿到受控的
		// file_gateway_write 凭据才开启，否则项目侧 verifyFileEvidence 因为没有
		// 客户端而返回 503。本地没有真实凭据（或不完整）时不写，不伪造机器身份，
		// 留给下一次受控更新补齐。
		if credential, ok := input.ServiceCredential(application.ServiceCredentialFileGatewayWrite); ok &&
			strings.TrimSpace(credential.OAuthClient.ClientID) != "" && strings.TrimSpace(credential.PlaintextSecret) != "" {
			values["FILE_GATEWAY_ENABLED"] = "true"
			values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
			values["FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
			values["FILE_GATEWAY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["FILE_GATEWAY_CLIENT_SECRET"] = credential.PlaintextSecret
			values["FILE_GATEWAY_SCOPE"] = "platform:file:upload platform:file:bind platform:file:download"
		}
	default:
		values = map[string]string{
			"APP_PUBLIC_URL":             input.PublicURL,
			"APP_PATH_PREFIX":            input.PathPrefix,
			"OIDC_REDIRECT_URI":          input.RedirectURI,
			"OIDC_SESSION_COOKIE_SECURE": secure,
		}
	}
	// 生命周期更新必须同时重放所选身份提供方。控制面的更新端点有意保留客户端 secret，
	// 因此这里只刷新 issuer 与 backchannel。
	backchannel := provisioner.oidcBackchannelBaseURL(input.Issuer)
	if input.ApplicationCode == integratedPortalApplicationCode {
		values["PORTAL_OIDC_ISSUER"] = input.Issuer
		if backchannel != "" {
			values["PORTAL_OIDC_BACKCHANNEL_BASE_URL"] = backchannel
		}
	} else {
		values["OIDC_ISSUER"] = input.Issuer
		if backchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = backchannel
		}
	}
	if input.ApplicationCode == integratedCustomerApplicationCode {
		values["OIDC_ROLE_CONFIG_HASH"] = provisioner.localRoleConfigHash("crm")
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		values["PORTAL_ROLE_CONFIG_HASH"] = provisioner.localRoleConfigHash("portal")
	}
	if err := preserveLocalBusinessIntegrationFlags(input.ApplicationCode, environmentPath, environmentPath, values); err != nil {
		return provisioningError("read subsystem public business source selection")
	}
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem public runtime configuration")
	}
	if input.ApplicationCode == integratedPortalApplicationCode && !localPortal {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
		if err := updateSubsystemEnvironment(customerEnvironment, customerEnvironment, map[string]string{
			"PORTAL_PUBLIC_URL": strings.TrimRight(input.PublicURL, "/"),
		}); err != nil {
			return provisioningError("write CRM Portal public runtime configuration")
		}
	}
	return nil
}

// updateOIDCRuntimeConfiguration 通过既有受控运行配置更新OIDC与统一会话接线。
// 参数：input为经过调用方边界校验的业务输入。
//
//	environmentPath为受控运行环境文件路径。
//
// 返回值：无；参数、校验、存储或依赖失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) updateOIDCRuntimeConfiguration(input application.SubsystemProvisioningInput, environmentPath string) error {
	values, policyErr := managedSessionEnvironment(input.CentralSessionPolicy, input.ApplicationCode)
	if policyErr != nil {
		return policyErr
	}
	backchannel := provisioner.oidcBackchannelBaseURL(input.Issuer)
	currentContent, readErr := os.ReadFile(environmentPath)
	if readErr != nil {
		return provisioningError("read subsystem OIDC runtime configuration")
	}
	current := parseEnvironmentValues(string(currentContent))
	issuerKey, clientIDKey, clientSecretKey := "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"
	if input.ApplicationCode == integratedPortalApplicationCode {
		issuerKey, clientIDKey, clientSecretKey = "PORTAL_OIDC_ISSUER", "PORTAL_OIDC_CLIENT_ID", "PORTAL_OIDC_CLIENT_SECRET"
		values["PORTAL_OIDC_ISSUER"] = input.Issuer
		if backchannel != "" {
			values["PORTAL_OIDC_BACKCHANNEL_BASE_URL"] = backchannel
		}
		values["PORTAL_ROLE_CONFIG_HASH"] = provisioner.localRoleConfigHash("portal")
	} else {
		values["OIDC_ISSUER"] = input.Issuer
		if backchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = backchannel
		}
		if input.ApplicationCode == integratedCustomerApplicationCode {
			values["OIDC_ROLE_CONFIG_HASH"] = provisioner.localRoleConfigHash("crm")
		}
	}
	rollbackClientIDKey, rollbackClientSecretKey, rollbackIssuerKey := clientIDKey+"_ROLLBACK", clientSecretKey+"_ROLLBACK", issuerKey+"_ROLLBACK"
	if input.AuthenticationRuntimeUpdate && strings.TrimSpace(input.ClientID) != "" && strings.TrimSpace(input.ClientSecret) != "" {
		if previousID, previousSecret := strings.TrimSpace(current[clientIDKey]), strings.TrimSpace(current[clientSecretKey]); previousID != "" && previousSecret != "" && previousID != input.ClientID {
			values[rollbackClientIDKey] = previousID
			values[rollbackClientSecretKey] = previousSecret
			values[rollbackIssuerKey] = strings.TrimSpace(current[issuerKey])
		}
		values[clientIDKey] = input.ClientID
		values[clientSecretKey] = input.ClientSecret
	} else if input.AuthenticationRuntimeUpdate && !strings.Contains(strings.ToLower(input.Issuer), "/realms/") {
		// 受控回滚恢复保留在 mode-0600 运行时文件中的上一个平台浏览器凭据；它绝不通过 API
		// 回传。
		if rollbackID, rollbackSecret := strings.TrimSpace(current[rollbackClientIDKey]), strings.TrimSpace(current[rollbackClientSecretKey]); rollbackID != "" && rollbackSecret != "" {
			values[clientIDKey] = rollbackID
			values[clientSecretKey] = rollbackSecret
		}
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		// SEC-D7b：Rebuild 写入路径同样补齐签署人手机号列加密密钥；已有合法键保留，不轮换。
		if err := contractNotificationRuntimeValues(input, values, false); err != nil {
			return err
		}
		if err := ensureContractSigningPhoneEnvironmentValue(environmentPath, values); err != nil {
			return err
		}
		if credential, ok := input.ServiceCredential(application.ServiceCredentialOwnerDirectoryRead); ok {
			values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"] = credential.PlaintextSecret
		}
	}
	// RETRY/ADOPT 会在重建前轮换一次性目录发布凭据。普通重启必须保持非破坏性；但一旦投递
	// 了新凭据，就必须原子替换完整目录身份，否则重建出来的应用可能沿旧 application
	// ID/secret，导致每次启动前的目录发布全部 401。
	if strings.TrimSpace(input.ApplicationID) != "" &&
		strings.TrimSpace(input.CatalogPublisherClientID) != "" &&
		strings.TrimSpace(input.CatalogPublisherClientSecret) != "" {
		if input.ApplicationCode == integratedPortalApplicationCode {
			values["PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID"] = strings.TrimSpace(input.ApplicationID)
			values["PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID"] = strings.TrimSpace(input.CatalogPublisherClientID)
			values["PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET"] = input.CatalogPublisherClientSecret
		} else {
			values["PLATFORM_APPLICATION_ID"] = strings.TrimSpace(input.ApplicationID)
			values["PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"] = strings.TrimSpace(input.ApplicationID)
			values["PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"] = strings.TrimSpace(input.CatalogPublisherClientID)
			values["PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"] = input.CatalogPublisherClientSecret
			if input.ApplicationCode == "data_analysis" {
				// dashboard-api 在启动期间自行发布目录；该开关必须在进程首次启动前打开，
				// 使初始管理员分配能从同一 application 解析 dashboard_admin。
				values["PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED"] = "true"
			}
		}
	}
	if strings.TrimSpace(input.TenantID) != "" {
		values["OIDC_TENANT_ID"] = strings.TrimSpace(input.TenantID)
	}
	if strings.TrimSpace(input.ApplicationID) != "" {
		values["PLATFORM_APPLICATION_ID"] = strings.TrimSpace(input.ApplicationID)
	}
	if strings.TrimSpace(input.ApplicationCode) != "" {
		values["PLATFORM_APPLICATION_CODE"] = strings.TrimSpace(input.ApplicationCode)
	}
	if strings.TrimSpace(input.Environment) != "" {
		values["PLATFORM_ENVIRONMENT_CODE"] = strings.TrimSpace(input.Environment)
	}
	if input.ApplicationCode == "data_analysis" {
		// 浏览器经公开 origin 访问 OIDC issuer，而容器必须经 Compose 私网发布其授权目录。
		// 这里若复用公开 issuer，localhost 会解析到 dashboard-api 自身，进程无法启动。
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
	}
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem OIDC runtime configuration")
	}
	return nil
}

// applyLocked 在部署互斥保护内执行已授权的接入请求。
//
//	applyLocked 包含 Provision 的实际写入与启动流程，调用方必须先持有互斥锁；环境文件采用
//	临时文件加原子替换，Compose 或网关失败时保留现场，便于诊断和安全重试；已明确选择的
//	独立业务来源不会被自动更新覆盖。
//
// 参数：ctx为调用上下文与取消边界。
//
//	input为经过调用方边界校验的业务输入。
//
// 返回值：无；参数、校验、存储或依赖失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) applyLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	// 与重建路径同一条 fail-closed 门禁：接入/接管同样不得在没有许可运行凭据时创建容器。
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	if err := provisioner.requireLocalLicenseRuntimeState(input); err != nil {
		return err
	}
	if err := provisioner.deliverLocalRuntimeLicense(ctx, input); err != nil {
		return err
	}

	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	projectDirectory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return err
	}
	composeFile := ""
	if !isIntegratedSubsystem(input.ApplicationCode) && len(input.RuntimeLicenseCredentials) == 0 {
		composeFile, err = locateComposeFile(projectDirectory)
		if err != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
	}
	environmentSource, environmentPath, err := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		if err := provisioner.validateRetainedContractRuntime(operationCtx, environmentPath); err != nil {
			return err
		}
	}
	publicOrigin, publicOriginErr := publicOriginFromURL(input.PublicURL)
	if publicOriginErr != nil {
		return provisioningError("subsystem public URL is invalid")
	}
	values := map[string]string{
		// PLATFORM_BASE_URL 是子系统回调平台控制面的私网地址；写成 OIDC issuer
		// （Keycloak realm URL）会让合同后端把认证指到 Keycloak 而不是平台，
		// 并被 docker-local 的运行时校验拒绝。
		"PLATFORM_BASE_URL":         "http://platform-api:8080",
		"OIDC_ISSUER":               input.Issuer,
		"OIDC_CLIENT_ID":            input.ClientID,
		"OIDC_CLIENT_SECRET":        input.ClientSecret,
		"OIDC_REDIRECT_URI":         input.RedirectURI,
		"OIDC_SCOPES":               "openid profile",
		"OIDC_TENANT_ID":            input.TenantID,
		"APP_PUBLIC_URL":            input.PublicURL,
		"APP_PATH_PREFIX":           input.PathPrefix,
		"PLATFORM_APPLICATION_ID":   input.ApplicationID,
		"PLATFORM_APPLICATION_CODE": input.ApplicationCode,
		"PLATFORM_ENVIRONMENT_CODE": input.Environment,
		// 本地接入在子系统启动后由隔离 provisioner 发布目录，不要把 contract-api 的可用性与
		// 该控制面操作耦合；隔离凭据只留给显式的对账流程使用。
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
		"PLATFORM_DOCKER_NETWORK":                       provisioner.config.PlatformDockerNetwork,
	}
	if input.ApplicationCode == "data_analysis" {
		// data_analysis 在 dashboard-api 开始服务前同步其内嵌目录，不能套用本地默认的
		// 不同步开关。
		values["PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED"] = "true"
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
	}
	// 这里构造的 map 是密钥进入运行时文件的唯一通道。后续写文件使用受限权限，命令行
	// 只传 env-file 路径，避免 secret 出现在进程列表或部署日志中。
	if input.ApplicationCode == integratedCustomerApplicationCode {
		// customer-api 编译进统一本地拓扑：OIDC 仍使用公开 issuer，但发现/令牌请求经 Compose
		// 私网别名路由。兼容哈希在启动时由应用内嵌目录推导，保证仅目录的发布不会与平台侧
		// 硬编码值漂移。
		values["APP_PUBLIC_ORIGIN"] = publicOrigin
		values["DEV_AUTH_ENABLED"] = "false"
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
		values["OIDC_BACKCHANNEL_BASE_URL"] = "http://platform-api:8080"
		if keycloakBackchannel := provisioner.oidcBackchannelBaseURL(input.Issuer); keycloakBackchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = keycloakBackchannel
		}
		values["OIDC_POST_LOGOUT_REDIRECT_URI"] = input.PublicURL
		values["OIDC_ROLE_CONFIG_HASH"] = provisioner.localRoleConfigHash("crm")
		values["OIDC_SESSION_COOKIE_SECURE"] = booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(publicOrigin), "https://"))
		auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		if !ok {
			return provisioningError("customer audit publisher credential is unavailable")
		}
		expectedAuditClientID := input.ApplicationCode + "-" + input.Environment + "-audit-publisher"
		if auditCredential.OAuthClient.ClientID != expectedAuditClientID {
			return provisioningError("customer audit publisher credential does not match the target environment")
		}
		values["PLATFORM_AUDIT_CLIENT_ID"] = auditCredential.OAuthClient.ClientID
		values["PLATFORM_AUDIT_CLIENT_SECRET"] = auditCredential.PlaintextSecret
		notificationCredential, ok := input.ServiceCredential(application.ServiceCredentialNotificationIngest)
		if !ok {
			return provisioningError("customer notification publisher credential is unavailable")
		}
		expectedNotificationClientID := input.ApplicationCode + "-" + input.Environment + "-notification-publisher"
		if notificationCredential.OAuthClient.ClientID != expectedNotificationClientID {
			return provisioningError("customer notification publisher credential does not match the target environment")
		}
		values["PLATFORM_NOTIFICATION_CLIENT_ID"] = notificationCredential.OAuthClient.ClientID
		values["PLATFORM_NOTIFICATION_CLIENT_SECRET"] = notificationCredential.PlaintextSecret
		ownerCredential, ok := input.ServiceCredential(application.ServiceCredentialOwnerDirectoryRead)
		if !ok {
			return provisioningError("customer owner directory credential is unavailable")
		}
		expectedOwnerClientID := input.ApplicationCode + "-" + input.Environment + "-owner-directory"
		if ownerCredential.OAuthClient.ClientID != expectedOwnerClientID {
			return provisioningError("customer owner directory credential does not match the target environment")
		}
		values["OWNER_DIRECTORY_ENABLED"] = "true"
		values["PLATFORM_OWNER_DIRECTORY_URL"] = "http://platform-api:8080/api/v1/internal/owner-directory"
		values["PLATFORM_MANAGEMENT_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["PLATFORM_OWNER_DIRECTORY_SCOPE"] = "owner_directory.read"
		values["PLATFORM_OWNER_DIRECTORY_CLIENT_ID"] = ownerCredential.OAuthClient.ClientID
		values["PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"] = ownerCredential.PlaintextSecret
		// 合同摘要核验与商机签约事件上报的客户端身份由客户应用分支下发：合同 API
		// 按 claims.Subject 比对 customer_and_opportunity-<env>-contract-summary/-opportunity-intake，
		// 合同应用自身的同名用途凭据不满足该验签约束。
		if err := customerContractIdentityRuntimeValues(input, values); err != nil {
			return err
		}
	}
	if input.ApplicationCode == integratedProjectApplicationCode {
		if err := projectNotificationRuntimeValues(input, values); err != nil {
			return err
		}
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
		values["OIDC_BACKCHANNEL_BASE_URL"] = "http://platform-api:8080"
		if keycloakBackchannel := provisioner.oidcBackchannelBaseURL(input.Issuer); keycloakBackchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = keycloakBackchannel
		}
		values["OIDC_POST_LOGOUT_REDIRECT_URI"] = input.PublicURL
		values["OIDC_SESSION_COOKIE_SECURE"] = booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(publicOrigin), "https://"))
		values["CONTRACT_INTEGRATION_ENABLED"] = "true"
		values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] = "true"
		values["CONTRACT_INTEGRATION_CLIENT_ID"] = integratedContractApplicationCode + "-" + input.Environment + "-project-integration"
		values["CONTRACT_INTEGRATION_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_INTEGRATION_ISSUER"] = "basic-platform"
		values["CONTRACT_INTEGRATION_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CONTRACT_INTEGRATION_CALLER_APPLICATION_CODE"] = "contract_management"
		values["CONTRACT_INTEGRATION_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CONTRACT_INTEGRATION_REQUIRED_SCOPE"] = "project.contract.import"
		auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		if !ok {
			return provisioningError("project audit publisher credential is unavailable")
		}
		values["PLATFORM_AUDIT_CLIENT_ID"] = auditCredential.OAuthClient.ClientID
		values["PLATFORM_AUDIT_CLIENT_SECRET"] = auditCredential.PlaintextSecret
		// 首次接入按受控凭据启用统一文件网关客户端；没有凭据时不写，项目侧
		// verifyFileEvidence 保持 503 失败关闭，而不是伪造写入身份。
		if credential, ok := input.ServiceCredential(application.ServiceCredentialFileGatewayWrite); ok &&
			strings.TrimSpace(credential.OAuthClient.ClientID) != "" && strings.TrimSpace(credential.PlaintextSecret) != "" {
			values["FILE_GATEWAY_ENABLED"] = "true"
			values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
			values["FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
			values["FILE_GATEWAY_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["FILE_GATEWAY_CLIENT_SECRET"] = credential.PlaintextSecret
			values["FILE_GATEWAY_SCOPE"] = "platform:file:upload platform:file:bind platform:file:download"
		}
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		localPortal, modeErr := localPortalModeFromFile(environmentSource)
		if modeErr != nil {
			return modeErr
		}
		credentials, credentialErr := portalRuntimeCredentials(input, localPortal)
		if credentialErr != nil {
			return credentialErr
		}
		values = map[string]string{
			"PORTAL_PUBLIC_ORIGIN":                        publicOrigin,
			"PORTAL_PATH_PREFIX":                          input.PathPrefix,
			"PORTAL_OIDC_ISSUER":                          input.Issuer,
			"PORTAL_OIDC_BACKCHANNEL_BASE_URL":            "http://platform-api:8080",
			"PORTAL_OIDC_CLIENT_ID":                       input.ClientID,
			"PORTAL_OIDC_CLIENT_SECRET":                   input.ClientSecret,
			"PORTAL_OIDC_REDIRECT_URI":                    input.RedirectURI,
			"PORTAL_OIDC_SCOPES":                          "openid profile",
			"PORTAL_OIDC_TENANT_ID":                       input.TenantID,
			"PORTAL_ROLE_CONFIG_HASH":                     provisioner.localRoleConfigHash("portal"),
			"PORTAL_SESSION_COOKIE_SECURE":                booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(publicOrigin), "https://")),
			"PORTAL_ACCOUNT_SECURITY_CENTER_URL":          strings.TrimRight(input.Issuer, "/") + "/settings/security",
			"PORTAL_MACHINE_TOKEN_ISSUER":                 "basic-platform",
			"PORTAL_MACHINE_TOKEN_AUDIENCE":               "basic-platform-application",
			"PORTAL_MACHINE_TOKEN_PUBLIC_KEY_PATH":        "/app/data/keys/jwt-ed25519-public.pem",
			"PORTAL_CRM_PROVISION_CLIENT_SUBJECT":         credentials[application.ServiceCredentialPortalMappingProvision].OAuthClient.ClientID,
			"PORTAL_CRM_DISABLE_CLIENT_SUBJECT":           credentials[application.ServiceCredentialPortalMappingDisable].OAuthClient.ClientID,
			"PORTAL_CRM_INVITE_BASE_URL":                  "http://customer-api:8090/customer-opportunity/api/v1",
			"PORTAL_CRM_INVITE_TOKEN_URL":                 "http://platform-api:8080/oauth2/token",
			"PORTAL_CRM_INVITE_CLIENT_ID":                 credentials[application.ServiceCredentialPortalInviteVerify].OAuthClient.ClientID,
			"PORTAL_CRM_INVITE_CLIENT_SECRET":             credentials[application.ServiceCredentialPortalInviteVerify].PlaintextSecret,
			"PORTAL_PLATFORM_BASE_URL":                    "http://platform-api:8080",
			"PORTAL_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
			"PORTAL_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
			"PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
			"PORTAL_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
			"PLATFORM_APPLICATION_CODE":                   integratedPortalApplicationCode,
			"PLATFORM_ENVIRONMENT_CODE":                   input.Environment,
			"PLATFORM_AUDIT_CLIENT_ID":                    credentials[application.ServiceCredentialAuditIngest].OAuthClient.ClientID,
			"PLATFORM_AUDIT_CLIENT_SECRET":                credentials[application.ServiceCredentialAuditIngest].PlaintextSecret,
		}
		// 逐键核对：运行文件中的机器凭据必须与本次受控交付完全一致。
		// 任何回退到历史 Secret 的路径都在这里失败关闭，而不是让进程带着旧值启动后 401。
		if err := verifyPortalDeliveredCredentials(input, values); err != nil {
			return err
		}
		if localPortal {
			values["PORTAL_CUSTOMER_DIRECTORY_MODE"] = "local"
		}
		if keycloakBackchannel := provisioner.oidcBackchannelBaseURL(input.Issuer); keycloakBackchannel != "" {
			values["PORTAL_OIDC_BACKCHANNEL_BASE_URL"] = keycloakBackchannel
		}
		runtimeSecrets, secretErr := portalRuntimeSecrets(environmentSource)
		if secretErr != nil {
			return provisioningError("prepare Portal runtime secrets")
		}
		for key, value := range runtimeSecrets {
			values[key] = value
		}
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
		portalURL := strings.TrimRight(input.PublicURL, "/")
		customerValues := map[string]string{
			"PORTAL_INVITE_ENABLED":                "true",
			"PORTAL_PUBLIC_URL":                    portalURL,
			"PORTAL_PROVISION_URL":                 "http://portal-api:8091/customer-portal/internal/accounts/provision",
			"PORTAL_PROVISION_TOKEN_URL":           "http://platform-api:8080/oauth2/token",
			"PORTAL_PROVISION_CLIENT_ID":           credentials[application.ServiceCredentialPortalMappingProvision].OAuthClient.ClientID,
			"PORTAL_PROVISION_CLIENT_SECRET":       credentials[application.ServiceCredentialPortalMappingProvision].PlaintextSecret,
			"PORTAL_DISABLE_URL":                   "http://portal-api:8091/customer-portal/internal/accounts/disable",
			"PORTAL_DISABLE_CLIENT_ID":             credentials[application.ServiceCredentialPortalMappingDisable].OAuthClient.ClientID,
			"PORTAL_DISABLE_CLIENT_SECRET":         credentials[application.ServiceCredentialPortalMappingDisable].PlaintextSecret,
			"PLATFORM_EXTERNAL_IDENTITY_ENABLED":   "true",
			"PLATFORM_EXTERNAL_USER_PROVISION_URL": "http://platform-api:8080/api/v1/internal/external-users",
			"PLATFORM_APPLICATION_ROLE_ASSIGN_URL": "http://platform-api:8080/api/v1/internal/application-roles",
			"PLATFORM_APPLICATION_ROLE_REVOKE_URL": "http://platform-api:8080/api/v1/internal/application-roles/revoke",
			"PLATFORM_MANAGEMENT_TOKEN_URL":        "http://platform-api:8080/oauth2/token",
			"PLATFORM_PORTAL_APPLICATION_CODE":     integratedPortalApplicationCode,
			"PLATFORM_EXTERNAL_USER_CLIENT_ID":     credentials[application.ServiceCredentialExternalUserProvision].OAuthClient.ClientID,
			"PLATFORM_EXTERNAL_USER_CLIENT_SECRET": credentials[application.ServiceCredentialExternalUserProvision].PlaintextSecret,
			"PLATFORM_ROLE_ASSIGN_CLIENT_ID":       credentials[application.ServiceCredentialApplicationRoleAssign].OAuthClient.ClientID,
			"PLATFORM_ROLE_ASSIGN_CLIENT_SECRET":   credentials[application.ServiceCredentialApplicationRoleAssign].PlaintextSecret,
			"PLATFORM_ROLE_REVOKE_CLIENT_ID":       credentials[application.ServiceCredentialApplicationRoleRevoke].OAuthClient.ClientID,
			"PLATFORM_ROLE_REVOKE_CLIENT_SECRET":   credentials[application.ServiceCredentialApplicationRoleRevoke].PlaintextSecret,
		}
		if !localPortal {
			if err := preserveLocalBusinessIntegrationFlags(input.ApplicationCode, environmentSource, customerEnvironment, customerValues); err != nil {
				return provisioningError("read CRM Portal business source selection")
			}
			if err := updateSubsystemEnvironment(customerEnvironment, customerEnvironment, customerValues); err != nil {
				return provisioningError("write CRM Portal integration configuration")
			}
		}
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		credentials, credentialErr := requiredContractServiceCredentials(input)
		if credentialErr != nil {
			return credentialErr
		}
		if err := contractNotificationRuntimeValues(input, values, true); err != nil {
			return err
		}
		// SEC-D7b：contract env 可能由模板 .env.example 首次生成（占位行/空值），Agent 必须在
		// 写入时补齐列加密密钥，否则 provisioner 重写后 /readyz 失败关闭；已有合法键保留。
		if err := ensureContractSigningPhoneEnvironmentValue(environmentPath, values); err != nil {
			return err
		}
		values["CONTRACT_MACHINE_TOKEN_ISSUER"] = "basic-platform"
		values["CONTRACT_MACHINE_TOKEN_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_MACHINE_TOKEN_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CRM_SIGNED_COUNT_MACHINE_ENABLED"] = "true"
		values["CRM_SIGNED_COUNT_MACHINE_REQUIRE_BEARER"] = "true"
		values["CRM_SIGNED_COUNT_MACHINE_ISSUER"] = "basic-platform"
		values["CRM_SIGNED_COUNT_MACHINE_AUDIENCE"] = "basic-platform-application"
		values["CRM_SIGNED_COUNT_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CRM_SIGNED_COUNT_MACHINE_CLIENT_ID"] = integratedCustomerApplicationCode + "-" + input.Environment + "-contract-signed-count"
		values["CRM_SIGNED_COUNT_MACHINE_CALLER_APPLICATION_CODE"] = integratedCustomerApplicationCode
		values["CRM_SIGNED_COUNT_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CRM_SIGNED_COUNT_MACHINE_REQUIRED_SCOPE"] = "contract.opportunity_signed_count.read"
		values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"] = credentials[application.ServiceCredentialOwnerDirectoryRead].OAuthClient.ClientID
		values["PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"] = credentials[application.ServiceCredentialOwnerDirectoryRead].PlaintextSecret
		values["CRM_REFERENCE_ENABLED"] = "true"
		values["CRM_REFERENCE_BASE_URL"] = "http://customer-api:8090/customer-opportunity"
		values["CRM_REFERENCE_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["CRM_REFERENCE_CLIENT_ID"] = credentials[application.ServiceCredentialCRMContractReferenceRead].OAuthClient.ClientID
		values["CRM_REFERENCE_CLIENT_SECRET"] = credentials[application.ServiceCredentialCRMContractReferenceRead].PlaintextSecret
		values["CRM_REFERENCE_SCOPE"] = "customer.contract_reference.read"
		values["PROJECT_INTEGRATION_ENABLED"] = "true"
		values["PROJECT_API_BASE_URL"] = "http://project-api:8082"
		values["PROJECT_INTEGRATION_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["PROJECT_INTEGRATION_CLIENT_ID"] = credentials[application.ServiceCredentialProjectContractImport].OAuthClient.ClientID
		values["PROJECT_INTEGRATION_CLIENT_SECRET"] = credentials[application.ServiceCredentialProjectContractImport].PlaintextSecret
		values["PROJECT_INTEGRATION_AUDIENCE"] = "basic-platform-application"
		// 首次接入同样要下发合同侧机器入口，否则本地新建栈需要先做一次受控更新
		// 才能复现 P0（商机确认签约）与 CRM 合同摘要核验。只写公开信任域参数，
		// client id 随 input.Environment 推导。
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_ENABLED"] = "true"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRE_BEARER"] = "true"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_ISSUER"] = "basic-platform"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_AUDIENCE"] = "basic-platform-application"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CLIENT_ID"] = integratedCustomerApplicationCode + "-" + input.Environment + "-opportunity-intake"
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_APPLICATION_CODE"] = integratedCustomerApplicationCode
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE"] = "opportunity.signed.write"
		values["CONTRACT_SUMMARY_MACHINE_ENABLED"] = "true"
		values["CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER"] = "true"
		values["CONTRACT_SUMMARY_MACHINE_ISSUER"] = "basic-platform"
		values["CONTRACT_SUMMARY_MACHINE_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
		values["CONTRACT_SUMMARY_MACHINE_CLIENT_ID"] = integratedCustomerApplicationCode + "-" + input.Environment + "-contract-summary"
		values["CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE"] = integratedCustomerApplicationCode
		values["CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
		values["CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE"] = "contract.summary.read"
		// CRM 回写默认关闭（地址留空 = 该边停用）：首次接入通常还没有回写凭据，
		// 写入占位值会让合同进程启动校验失败关闭。
		values["CRM_LINK_BASE_URL"] = ""
		if credential, ok := input.ServiceCredential(application.ServiceCredentialCRMContractLinkWrite); ok &&
			strings.TrimSpace(credential.OAuthClient.ClientID) != "" && strings.TrimSpace(credential.PlaintextSecret) != "" {
			values["CRM_LINK_BASE_URL"] = "http://customer-api:8090/customer-opportunity"
			values["CRM_LINK_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
			values["CRM_LINK_CLIENT_ID"] = credential.OAuthClient.ClientID
			values["CRM_LINK_CLIENT_SECRET"] = credential.PlaintextSecret
			values["CRM_LINK_SCOPE"] = "opportunity.signed.write"
		}
		// 合同摘要核验/商机签约事件的客户端身份归属客户应用（见
		// customerContractIdentityRuntimeValues）。这里有意不再向 .env.customer.local 写
		// CONTRACT_* 的 client id/secret：合同应用自身的凭据不满足 claims.Subject 要求，
		// 覆盖只会让本地 P0 恒 401；合同分支也不写 verification 开关，避免客户应用在
		// 尚未下发凭据时因 CONTRACT_VERIFICATION_ENABLED=true 启动校验失败关闭。
	}
	if err := provisioner.p1LocalRuntimeValues(input, environmentPath, values, true); err != nil {
		return err
	}
	policyValues, policyErr := managedSessionEnvironment(input.CentralSessionPolicy, input.ApplicationCode)
	if policyErr != nil {
		return policyErr
	}
	for key, value := range policyValues {
		values[key] = value
	}
	// 统一会话值写入后再保留运维已明确选择的独立业务来源：两者键空间不重叠，
	// 保留逻辑只覆盖白名单业务开关，不改变平台会话开关。
	if err := preserveLocalBusinessIntegrationFlags(input.ApplicationCode, environmentSource, environmentSource, values); err != nil {
		return provisioningError("read adopted subsystem business source selection")
	}
	if err := updateSubsystemEnvironment(environmentSource, environmentPath, values); err != nil {
		return provisioningError("write subsystem environment file")
	}
	if input.ApplicationCode == integratedContractApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		platformEnvironmentPath := filepath.Join(platformRoot, "docker", ".env.local")
		if err := updateSubsystemEnvironment(platformEnvironmentPath, platformEnvironmentPath, map[string]string{
			"CONTRACT_CATALOG_PUBLISHER_CLIENT_ID":     input.CatalogPublisherClientID,
			"CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET": input.CatalogPublisherClientSecret,
		}); err != nil {
			return provisioningError("write contract catalog publisher operations configuration")
		}
	}

	// 生成的文件始终是 .env.local，这是子系统 Compose 的既定约定；--env-file 同时让这些值
	// 参与 Compose 插值。
	var startErr error
	switch {
	case len(input.RuntimeLicenseCredentials) > 0:
		startErr = provisioner.deployLicensedLocalStack(operationCtx, input)
	case input.ApplicationCode == integratedContractApplicationCode:
		startErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case input.ApplicationCode == integratedCustomerApplicationCode:
		startErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case input.ApplicationCode == integratedPortalApplicationCode:
		startErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	case input.ApplicationCode == "reimbursement":
		startErr = provisioner.rebuildIntegratedReimbursementStack(operationCtx)
	case input.ApplicationCode == integratedProjectApplicationCode:
		startErr = provisioner.rebuildIntegratedProjectStack(operationCtx)
	default:
		startErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
	}
	if startErr != nil {
		return localRuntimeDeploymentError(startErr, "start subsystem containers")
	}

	// contract_management 编译进统一前端，其 API 路由已在 nginx/default.conf 中；若在 nginx
	// 正在代理本次接入请求时 reload 同一实例，可能在成功返回 201 之前断开客户端上游连接。
	// 只有独立部署的子系统才需要生成整站代理条目。
	if !isIntegratedSubsystem(input.ApplicationCode) && len(input.RuntimeLicenseCredentials) == 0 {
		gatewayEnvironment := append(os.Environ(), "PORTAL_GATEWAY_NGINX_INCLUDE="+provisioner.config.GatewayIncludePath)
		if err := provisioner.runner.Run(operationCtx, filepath.Dir(provisioner.config.GatewayScriptPath), gatewayEnvironment,
			"/bin/bash", provisioner.config.GatewayScriptPath, "add", input.ApplicationCode, input.PathPrefix, input.UpstreamURL); err != nil {
			return provisioningError("update portal gateway configuration")
		}

		containerID, err := provisioner.frontendContainerID(operationCtx, projectDirectory)
		if err != nil {
			return err
		}
		if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-t"); err != nil {
			return provisioningError("validate portal gateway configuration")
		}
		if err := provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-s", "reload"); err != nil {
			return provisioningError("reload portal gateway")
		}
	}

	// 接入后的授权目录同步。钩子只对配置的目标应用（当前为 contract_management）触发，且
	// 仅在运维提供了所需的镜像 + 数据库坐标时执行。脚本是尽力而为：失败只记日志、不阻塞
	// 接入响应，避免种子数据缺少发布客户端时把新子系统卡在无法恢复的状态。
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		if provisioner.config.ProjectsRoot == "" {
			// runner 内有意不持有 logger；此处降级为标准错误输出。
			fmt.Fprintf(os.Stderr, "[subsystem-provisioner] contract catalog sync skipped or failed: %v\n", err)
		}
	}
	return nil
}

func requiredContractServiceCredentials(input application.SubsystemProvisioningInput) (map[string]application.SubsystemServiceCredential, error) {
	purposes := []string{
		application.ServiceCredentialAuditIngest,
		application.ServiceCredentialNotificationIngest,
		application.ServiceCredentialContractOpportunitySignedWrite,
		application.ServiceCredentialContractSummaryRead,
		application.ServiceCredentialOwnerDirectoryRead,
		application.ServiceCredentialCRMContractReferenceRead,
		application.ServiceCredentialProjectContractImport,
	}
	result := make(map[string]application.SubsystemServiceCredential, len(purposes))
	for _, purpose := range purposes {
		credential, ok := input.ServiceCredential(purpose)
		if !ok {
			return nil, provisioningError("contract service credentials are incomplete")
		}
		result[purpose] = credential
	}
	return result, nil
}

// contractNotificationRuntimeValues 写入合同通知发布方的运行时键，只有合同通知发布方凭据
// 可以填充这些键。非凭据重启缺少输入时保留既有运行时值；受控更新会显式重新投递其只写
// 密钥。
func contractNotificationRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string, required bool) error {
	credential, ok := input.ServiceCredential(application.ServiceCredentialNotificationIngest)
	if !ok {
		for _, candidate := range input.ServiceCredentials {
			if candidate.Purpose == application.ServiceCredentialNotificationIngest {
				return provisioningError("contract notification publisher credential is incomplete")
			}
		}
		if required {
			return provisioningError("contract notification publisher credential is unavailable")
		}
		return nil
	}
	client := credential.OAuthClient
	// 环境 ID 由控制面凭据对账器校验；其部署 DTO 只携带目标环境编码。
	if input.TenantID == "" || input.ApplicationID == "" || client.EnvironmentID == "" ||
		client.TenantID != input.TenantID || client.ApplicationID != input.ApplicationID ||
		client.ClientID != input.ApplicationCode+"-"+input.Environment+"-notification-publisher" || client.Status != "ACTIVE" ||
		client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" ||
		len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" || len(client.Scopes) != 1 || client.Scopes[0] != "notification.ingest" {
		return provisioningError("contract notification publisher credential does not match the target environment or purpose")
	}
	values["PLATFORM_NOTIFICATION_CLIENT_ID"] = client.ClientID
	values["PLATFORM_NOTIFICATION_CLIENT_SECRET"] = credential.PlaintextSecret
	return nil
}

// rebuildIntegratedContractStack 让 contract_management 保持在工作区唯一的本地 Compose
// 拓扑中。统一前端已经路由到 basic-platform-local/contract-api；若再启动子系统的独立
// Compose 文件，会创建第二个由不同 MySQL 卷支撑的 contract-api 网络别名，请求结果不确定。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedContractStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "contract-mysql", "temporal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "contract-migrate"); err != nil {
		return err
	}
	// 先构建镜像，从该确切镜像发布目录，最后才替换 API。避免新二进制带着旧的 N/N-1 目录
	// 窗口对外服务，把所有 OIDC 回调变成 local_authorization 401。
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "contract-api", "contract-worker"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "contract-api", "./authz-catalog", "publish"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "contract-api", "contract-worker")
}

// rebuildIntegratedProjectStack 让 project_management 保持在工作区唯一的本地 Compose
// 拓扑中。统一前端已经路由到 basic-platform-local/project-api；若再启动子系统的独立
// Compose 文件，会创建第二个由不同 MySQL 卷支撑的 project-api 网络别名，请求结果不确定。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedProjectStack(ctx context.Context) error {
	// Building only prepares a candidate; it never replaces a running consumer.
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "project-api", "sla-notifier"); err != nil {
		return err
	}
	background, err := provisioner.projectBackgroundTarget(ctx)
	if err != nil {
		return err
	}
	if err := background.preflight(ctx); err != nil {
		return err
	}
	if err := prepareProjectBackground(ctx, background); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "project-mysql", "temporal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "project-migrate"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "project-api", "sla-notifier"); err != nil {
		return err
	}
	return activateProjectBackground(ctx, background)
}

// rebuildIntegratedSettlementStack 把 Settlement 保持在唯一的平台 Compose 项目内。步骤有意
// 限定在其自身的数据库、迁移、API 与 Worker，不拆除共享服务，也不写 Settlement 运行时
// 密钥。运行时收编（ADOPT）绝不构建镜像：部署 Agent 可能处于网络隔离状态，收编的目的
// 是验证已发布的本地镜像，而不是发布新版本。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedSettlementStack(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "settlement-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "settlement-migrate"); err != nil {
		return err
	}
	// 在替换任一运行进程之前发布应用自有的目录。平台会保留紧邻上一版目录/哈希作为兼容
	// 窗口，因此先发布 vN 既让旧的 vN-1 进程保持授权，也确保新进程不会在不兼容的平台目录
	// 上启动。发布失败时既有 API/Worker 原样保留，避免暴露一个拒绝所有授权上下文的半升级
	// 子系统。
	if err := provisioner.syncSettlementCatalog(ctx, input); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-build", "--no-deps", "settlement-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--no-build", "--no-deps", "settlement-worker"); err != nil {
		return err
	}
	return nil
}

func (provisioner *LocalDockerSubsystemProvisioner) updateSettlementCatalogRuntimeConfiguration(input application.SubsystemProvisioningInput) error {
	// 普通重启不得铸造或覆盖发布者 secret；ADOPT 与 RETRY 在到达此处前已显式投递新的一次性
	// 密钥。
	if strings.TrimSpace(input.CatalogPublisherClientID) == "" || strings.TrimSpace(input.CatalogPublisherClientSecret) == "" {
		return nil
	}
	projectDirectory, err := provisioner.projectDirectory(integratedSettlementApplicationCode)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(projectDirectory, ".env.local")
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, map[string]string{
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "true",
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": strings.TrimSpace(input.ApplicationID),
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      strings.TrimSpace(input.CatalogPublisherClientID),
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
	}); err != nil {
		return provisioningError("write settlement catalog publisher configuration")
	}
	return nil
}

func (provisioner *LocalDockerSubsystemProvisioner) syncSettlementCatalog(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if strings.TrimSpace(input.ApplicationID) == "" || strings.TrimSpace(input.CatalogPublisherClientID) == "" || strings.TrimSpace(input.CatalogPublisherClientSecret) == "" {
		// 普通重启没有明文发布者凭据，不能被当作目录发布尝试。
		return nil
	}
	projectDirectory, err := provisioner.projectDirectory(integratedSettlementApplicationCode)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(projectDirectory, "authz", "permission-manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		return provisioningError("Settlement authorization catalog manifest is unavailable")
	}
	runnerEnvironment := append(os.Environ(),
		"PLATFORM_APPLICATION_ID="+input.ApplicationID,
		// 这是控制面 API 端点而非 OIDC issuer。一次性发布容器在下方加入平台 Compose 网络，
		// 因此即便认证方是 Keycloak，也能使用稳定的服务别名。
		"PLATFORM_BASE_URL=http://platform-api:8080",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID="+input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET="+input.CatalogPublisherClientSecret,
	)
	return provisioner.runner.Run(ctx, projectDirectory, runnerEnvironment, provisioner.config.DockerBinary,
		"run", "--rm", "--network="+provisioner.config.PlatformDockerNetwork,
		"-v", manifestPath+":/catalog/permission-manifest.json:ro",
		"-e", "PLATFORM_APPLICATION_ID", "-e", "PLATFORM_BASE_URL",
		"-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID", "-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
		provisioner.config.CatalogSyncImage, "/usr/local/bin/sync-settlement-catalog.sh",
	)
}

// rebuildIntegratedCustomerStack 在以 OIDC 模式启动 customer-api 之前发布应用自有的授权
// 目录。目录发布是一次性部署操作，普通 API 重启因此不依赖长期有效的发布者凭据。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedCustomerStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "customer-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "customer-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "customer-migrate"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "customer-api", "./authz-catalog", "publish", "crm"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "customer-api")
}

// rebuildIntegratedPortalStack 构建门户自有进程，显式本地目录不重建CRM业务。
// 参数：ctx控制审核编排步骤，来源只读门户运行文件。
// 返回值：配置或命令错误；旧CRM模式仍在目录发布后重建客户应用。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedPortalStack(ctx context.Context) error {
	platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
	localPortal, err := localPortalModeFromFile(filepath.Join(platformRoot, "docker/.env.portal.local"))
	if err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "portal-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "build", "portal-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "portal-migrate"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "portal-api", "./authz-catalog", "publish", "portal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "portal-api"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--no-deps", "portal-report-worker"); err != nil {
		return err
	}
	if localPortal {
		return nil
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--force-recreate", "--no-deps", "customer-api")
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformCompose(ctx context.Context, arguments ...string) error {
	_, err := provisioner.executeIntegratedPlatformCompose(ctx, false, arguments...)
	return err
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformComposeOutput(ctx context.Context, arguments ...string) ([]byte, error) {
	return provisioner.executeIntegratedPlatformCompose(ctx, true, arguments...)
}

// executeIntegratedPlatformCompose 按本次操作所选服务校验运行文件并执行统一编排。
// 参数：ctx 控制期限，capture 表示需要输出，arguments 为受信内部 Compose 操作参数。
// 返回值：捕获的命令输出；配置、许可覆盖或命令失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) executeIntegratedPlatformCompose(ctx context.Context, capture bool, arguments ...string) ([]byte, error) {
	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, err := provisioner.integratedComposeConfiguration(arguments...)
	if err != nil {
		return nil, err
	}
	required := integratedRuntimeTargets(arguments)
	settlementEnvironment, err := integratedRuntimeEnvironment(
		filepath.Join(workspaceRoot, "Settlement", ".env.local"), required["settlement"])
	if err != nil {
		return nil, err
	}
	reimbursementEnvironment, reimbursementInterpolation, err := reimbursementComposeEnvironment(
		filepath.Join(platformRoot, "docker", ".env.reimbursement.local"), required["reimbursement"])
	if err != nil {
		return nil, err
	}
	// 集成子系统重建必须确定性执行，不得继承启动平台 API 的 shell 中残留的 LAN 覆盖值。
	// 常规 docker-local 入口会显式设置这些值；provisioner 从 API 容器内部运行 Compose，必须
	// 做同样的事。否则 Compose 可能解析到旧的或缺失的 platform/docker/.env.lan，把
	// contract-api 卡在 Created 状态。
	lanPlaceholder := filepath.Join(platformRoot, "docker", ".env.lan.disabled")
	analysisEnvironment, err := integratedRuntimeEnvironment(
		filepath.Join(workspaceRoot, "data_analysis", ".env.local"), required["data_analysis"])
	if err != nil {
		return nil, err
	}
	composeArguments := []string{
		"compose", "--project-name", provisioner.config.PlatformComposeProject,
		"--project-directory", platformRoot,
		"--env-file", platformEnvironment,
		"--env-file", contractEnvironment,
		"--env-file", customerEnvironment,
		"--env-file", portalEnvironment,
		"--env-file", projectEnvironment,
		"--env-file", settlementEnvironment,
		"--env-file", analysisEnvironment,
		"-f", composeFile,
	}
	// 集成拓扑里报销等服务受 compose profile 控制：不显式启用 profile 时，
	// 重建路径会以 no such service 拒绝 reimbursement-mysql/migrate/api/worker。
	composeArguments = append(composeArguments, localLicenseComposeProfileArgs()...)
	if !slices.Contains(arguments, "build") {
		if override, err := provisioner.localRuntimeOverride(); err != nil {
			return nil, err
		} else if override != "" {
			composeArguments = append(composeArguments, "-f", override)
		}
	}
	composeArguments = append(composeArguments, arguments...)
	lanOverrideKeys := map[string]struct{}{
		"BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE": {},
		"CONTRACT_LAN_OVERRIDE_ENV_FILE":       {},
		"CUSTOMER_LAN_OVERRIDE_ENV_FILE":       {},
		"PORTAL_LAN_OVERRIDE_ENV_FILE":         {},
		"PROJECT_LAN_OVERRIDE_ENV_FILE":        {},
	}
	subsystemEnvironmentKeys := map[string]struct{}{
		"CONTRACT_SUBSYSTEM_ENVIRONMENT":      {},
		"CUSTOMER_SUBSYSTEM_ENVIRONMENT":      {},
		"PORTAL_SUBSYSTEM_ENVIRONMENT":        {},
		"PROJECT_SUBSYSTEM_ENVIRONMENT":       {},
		"DATA_ANALYSIS_SUBSYSTEM_ENVIRONMENT": {},
		"SETTLEMENT_SUBSYSTEM_ENVIRONMENT":    {},
		"REIMBURSEMENT_SUBSYSTEM_ENVIRONMENT": {},
		"REIMBURSEMENT_MYSQL_PASSWORD":        {},
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD":   {},
		"REIMBURSEMENT_RUNTIME_ENV_FILE":      {},
	}
	workerEnvironment, err := integratedRuntimeEnvironment(
		filepath.Join(platformRoot, "docker", ".env.portal-report-worker.local"), required["portal_report"])
	if err != nil {
		return nil, err
	}
	presaleEnvironment, err := integratedRuntimeEnvironment(
		filepath.Join(workspaceRoot, integratedCustomerApplicationCode, ".env.presale-worker"), required["presale"])
	if err != nil {
		return nil, err
	}
	runnerEnvironment := make([]string, 0, len(os.Environ())+12)
	runnerEnvironment = append(runnerEnvironment, "PORTAL_REPORT_WORKER_RUNTIME_ENV_FILE="+workerEnvironment)
	runnerEnvironment = append(runnerEnvironment, reimbursementInterpolation...)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, isLANOverride := lanOverrideKeys[key]; isLANOverride {
				continue
			}
			if _, isSubsystemEnvironment := subsystemEnvironmentKeys[key]; isSubsystemEnvironment {
				continue
			}
		}
		runnerEnvironment = append(runnerEnvironment, entry)
	}
	runnerEnvironment = append(runnerEnvironment,
		"BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="+lanPlaceholder,
		"CONTRACT_LAN_OVERRIDE_ENV_FILE="+lanPlaceholder,
		"CUSTOMER_LAN_OVERRIDE_ENV_FILE="+lanPlaceholder,
		"PORTAL_LAN_OVERRIDE_ENV_FILE="+lanPlaceholder,
		"PROJECT_LAN_OVERRIDE_ENV_FILE="+lanPlaceholder,
	)
	runnerEnvironment = append(runnerEnvironment,
		"BASIC_PLATFORM_RUNTIME_ENV_FILE="+platformEnvironment,
		"CONTRACT_RUNTIME_ENV_FILE="+contractEnvironment,
		"CUSTOMER_RUNTIME_ENV_FILE="+customerEnvironment,
		"PORTAL_RUNTIME_ENV_FILE="+portalEnvironment,
		"PROJECT_RUNTIME_ENV_FILE="+projectEnvironment,
		"SETTLEMENT_RUNTIME_ENV_FILE="+settlementEnvironment,
		"REIMBURSEMENT_RUNTIME_ENV_FILE="+reimbursementEnvironment,
		"DATA_ANALYSIS_RUNTIME_ENV_FILE="+analysisEnvironment,
		"PRESALE_WORKER_ENV_FILE="+presaleEnvironment,
		"BASIC_PLATFORM_HOST_PROJECT_ROOT="+platformRoot,
		"SUBSYSTEM_HOST_PROJECTS_ROOT="+workspaceRoot,
	)
	// Provision 在调用 Compose 之前把所选环境写入运行时文件。每次操作都重新读取这些文件，
	// 让首次 dev/test/staging/prod 接入无需重启 provisioner 就能拿到匹配的发现 label。
	runtimeEnvironments := []struct {
		key, path, applicationCode, clientKey string
	}{
		{"CONTRACT_SUBSYSTEM_ENVIRONMENT", contractEnvironment, integratedContractApplicationCode, "OIDC_CLIENT_ID"},
		{"CUSTOMER_SUBSYSTEM_ENVIRONMENT", customerEnvironment, integratedCustomerApplicationCode, "OIDC_CLIENT_ID"},
		{"PORTAL_SUBSYSTEM_ENVIRONMENT", portalEnvironment, integratedPortalApplicationCode, "PORTAL_OIDC_CLIENT_ID"},
		{"PROJECT_SUBSYSTEM_ENVIRONMENT", projectEnvironment, integratedProjectApplicationCode, "OIDC_CLIENT_ID"},
		{"DATA_ANALYSIS_SUBSYSTEM_ENVIRONMENT", filepath.Join(workspaceRoot, "data_analysis", ".env.local"), "data_analysis", "OIDC_CLIENT_ID"},
		{"SETTLEMENT_SUBSYSTEM_ENVIRONMENT", settlementEnvironment, "settlement", "OIDC_CLIENT_ID"},
		{"REIMBURSEMENT_SUBSYSTEM_ENVIRONMENT", reimbursementEnvironment, "reimbursement", "OIDC_CLIENT_ID"},
	}
	for _, target := range runtimeEnvironments {
		environment := localRuntimeEnvironment(target.path, target.applicationCode, target.clientKey)
		if environment == "" {
			environment = "dev"
		}
		runnerEnvironment = append(runnerEnvironment, target.key+"="+environment)
	}
	if !slices.Contains(arguments, "build") && (slices.Contains(arguments, "project-api") || slices.Contains(arguments, "sla-notifier") || slices.Contains(arguments, "project-migrate")) {
		image, err := provisioner.localProjectBackgroundImage(ctx)
		if err != nil {
			return nil, err
		}
		runnerEnvironment = append(runnerEnvironment, "PROJECT_BACKGROUND_BUILD_ID=project-background-sha256-"+strings.TrimPrefix(image, "sha256:"))
	}
	if capture {
		runner, ok := provisioner.runner.(interface {
			RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
		})
		if !ok {
			return nil, errors.New("local Compose inspection output unavailable")
		}
		return runner.RunOutput(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
	}
	outputRunner, ok := provisioner.runner.(subsystemCommandOutputRunner)
	if !ok {
		return nil, provisioner.runner.Run(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
	}
	output, runErr := outputRunner.RunOutput(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
	if runErr == nil {
		return nil, nil
	}
	// 下层原因必须随错误一起返回：平台把外层短语写进 last_error_message 时，运维只剩
	// 一句无信息量的“部署 Agent 执行失败”，只能反复重试。
	if reason := provisioner.localComposeFailureReason(ctx, composeArguments, output); reason != "" {
		return nil, fmt.Errorf("%w: %s", runErr, reason)
	}
	return nil, runErr
}

func localRuntimeEnvironment(path, applicationCode, clientKey string) string {
	values, err := readEnvironmentValues(path)
	if err != nil {
		return ""
	}
	if environment := strings.ToLower(strings.TrimSpace(values["PLATFORM_ENVIRONMENT_CODE"])); validLocalRuntimeEnvironment(environment) {
		return environment
	}
	clientID := strings.TrimSpace(values[clientKey])
	prefix, suffix := strings.TrimSpace(applicationCode)+"-", "-web"
	if strings.HasPrefix(clientID, prefix) && strings.HasSuffix(clientID, suffix) {
		environment := strings.TrimSuffix(strings.TrimPrefix(clientID, prefix), suffix)
		if validLocalRuntimeEnvironment(environment) {
			return environment
		}
	}
	return ""
}

func validLocalRuntimeEnvironment(value string) bool {
	switch value {
	case "dev", "test", "staging", "prod":
		return true
	default:
		return false
	}
}

// integratedRuntimeTargets 标识本次会实际运行应用进程的模块，不将共享基础设施当作购买业务。
// 参数：arguments 为受信的内部 Compose 操作与服务参数。
// 返回值：必须具有自身运行文件的模块集合；预检、构建及只读操作返回空集合。
func integratedRuntimeTargets(arguments []string) map[string]bool {
	required := make(map[string]bool)
	if len(arguments) == 0 || !slices.Contains([]string{"up", "run", "exec"}, arguments[0]) {
		return required
	}
	for _, service := range arguments[1:] {
		switch {
		case service == "contract-mysql", service == "temporal":
			// 合同命名 MySQL 仍承载共享 Temporal，不能据此要求合同业务配置。
		case service == "contract-transfer-worker", service == "portal-invite-compensation-worker":
			required["customer"] = true
		case service == "portal-report-worker":
			required["portal_report"] = true
		case service == "presale-worker":
			required["presale"] = true
		case strings.HasPrefix(service, "contract-"):
			required["contract"] = true
		case strings.HasPrefix(service, "customer-"):
			required["customer"] = true
		case strings.HasPrefix(service, "portal-"):
			required["portal"] = true
		case strings.HasPrefix(service, "project-"), service == "sla-notifier":
			required["project"] = true
		case strings.HasPrefix(service, "reimbursement-"):
			required["reimbursement"] = true
		case strings.HasPrefix(service, "settlement-"):
			required["settlement"] = true
		case strings.HasPrefix(service, "dashboard-"), service == "aggregation-worker", service == "alert-worker":
			required["data_analysis"] = true
		}
	}
	return required
}

// integratedRuntimeEnvironment 区分未选模块的解析占位与实际运行所需的真实配置。
// 参数：path 为运维控制的环境文件路径，required 表示本次会运行该模块进程。
// 返回值：真实文件或未选模块的空解析文件；必需配置缺失、不合法或无法核验时返回错误。
func integratedRuntimeEnvironment(path string, required bool) (string, error) {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	if required || (err != nil && !errors.Is(err, os.ErrNotExist)) {
		return "", provisioningError("selected subsystem runtime environment is unavailable")
	}
	return os.DevNull, nil
}

// integratedComposeConfiguration 校验平台文件，并按本次操作解析各业务运行文件。
// 参数：arguments 为可选 Compose 操作；无参数表示只校验平台与解析现有模块配置。
// 返回值：平台/工作区/编排及四业务环境路径；平台文件或选中业务配置缺失时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) integratedComposeConfiguration(arguments ...string) (string, string, string, string, string, string, string, string, error) {
	platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
	workspaceRoot := filepath.Dir(platformRoot)
	composeFile := provisioner.localComposeFile()
	platformEnvironment := filepath.Join(platformRoot, "docker", ".env.local")
	for _, required := range []string{composeFile, platformEnvironment} {
		if info, err := os.Stat(required); err != nil || !info.Mode().IsRegular() {
			return "", "", "", "", "", "", "", "", provisioningError("integrated subsystem Compose configuration is unavailable")
		}
	}
	required := integratedRuntimeTargets(arguments)
	paths := []struct{ module, path string }{
		{"contract", filepath.Join(workspaceRoot, integratedContractApplicationCode, ".env.local")},
		{"customer", filepath.Join(platformRoot, "docker", ".env.customer.local")},
		{"portal", filepath.Join(platformRoot, "docker", ".env.portal.local")},
		{"project", filepath.Join(workspaceRoot, integratedProjectApplicationCode, ".env.local")},
	}
	for index := range paths {
		path, err := integratedRuntimeEnvironment(paths[index].path, required[paths[index].module])
		if err != nil {
			return "", "", "", "", "", "", "", "", err
		}
		paths[index].path = path
	}
	return platformRoot, workspaceRoot, composeFile, platformEnvironment,
		paths[0].path, paths[1].path, paths[2].path, paths[3].path, nil
}

// subsystemEnvironmentPaths 解析受控应用模板和目标运行文件。
// 参数：applicationCode为应用码，projectDirectory为已核验目录。
// 返回值：模板源和目标路径；来源缺失时返回错误，不自行寻找业务配置替代。
func (provisioner *LocalDockerSubsystemProvisioner) subsystemEnvironmentPaths(applicationCode, projectDirectory string) (string, string, error) {
	if applicationCode == "reimbursement" {
		root := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		destination := filepath.Join(root, "docker", ".env.reimbursement.local")
		if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() {
			return destination, destination, nil
		}
		source := filepath.Join(root, "docker", ".env.reimbursement.local.example")
		if info, err := os.Stat(source); err == nil && info.Mode().IsRegular() {
			return source, destination, nil
		}
		return "", "", os.ErrNotExist
	}

	if applicationCode == integratedCustomerApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		destination := filepath.Join(platformRoot, "docker", ".env.customer.local")
		if info, err := os.Stat(destination); err == nil && !info.IsDir() {
			return destination, destination, nil
		}
		source := filepath.Join(platformRoot, "docker", ".env.customer.local.example")
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			return source, destination, nil
		}
		return "", "", os.ErrNotExist
	}
	if applicationCode == integratedPortalApplicationCode {
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		destination := filepath.Join(platformRoot, "docker", ".env.portal.local")
		if info, err := os.Stat(destination); err == nil && !info.IsDir() {
			return destination, destination, nil
		}
		source := filepath.Join(platformRoot, "docker", ".env.portal.local.example")
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			return source, destination, nil
		}
		return "", "", os.ErrNotExist
	}
	source, err := locateEnvironmentSource(projectDirectory)
	if err != nil {
		return "", "", err
	}
	return source, filepath.Join(projectDirectory, ".env.local"), nil
}

// isIntegratedSubsystem 判断应用是否由平台统一Compose托管。
// 参数：applicationCode为已规范化应用码。
// 返回值：受控集成应用返回true。
func isIntegratedSubsystem(applicationCode string) bool {
	switch strings.TrimSpace(applicationCode) {
	case "reimbursement", integratedContractApplicationCode, integratedCustomerApplicationCode, integratedPortalApplicationCode, integratedProjectApplicationCode, integratedSettlementApplicationCode:
		return true
	default:
		return false
	}
}

func requiredPortalServiceCredentials(input application.SubsystemProvisioningInput) (map[string]application.SubsystemServiceCredential, error) {
	purposes := []string{
		application.ServiceCredentialAuditIngest,
		application.ServiceCredentialExternalUserProvision,
		application.ServiceCredentialApplicationRoleAssign,
		application.ServiceCredentialApplicationRoleRevoke,
		application.ServiceCredentialPortalMappingProvision,
		application.ServiceCredentialPortalMappingDisable,
		application.ServiceCredentialPortalInviteVerify,
	}
	result := make(map[string]application.SubsystemServiceCredential, len(purposes))
	for _, purpose := range purposes {
		credential, ok := input.ServiceCredential(purpose)
		if !ok {
			return nil, provisioningError("customer Portal service credentials are incomplete")
		}
		result[purpose] = credential
	}
	return result, nil
}

// verifyPortalDeliveredCredentials 确保门户运行配置里的机器凭据逐项等于本次受控交付的
// client_id 与明文 Secret。它把"沿用历史值"从静默降级变成部署期可见失败。
func verifyPortalDeliveredCredentials(input application.SubsystemProvisioningInput, values map[string]string) error {
	pairs := []struct{ purpose, idKey, secretKey string }{
		{application.ServiceCredentialAuditIngest, "PLATFORM_AUDIT_CLIENT_ID", "PLATFORM_AUDIT_CLIENT_SECRET"},
		{application.ServiceCredentialPortalInviteVerify, "PORTAL_CRM_INVITE_CLIENT_ID", "PORTAL_CRM_INVITE_CLIENT_SECRET"},
	}
	for _, pair := range pairs {
		credential, ok := input.ServiceCredential(pair.purpose)
		if !ok || strings.TrimSpace(credential.PlaintextSecret) == "" {
			// 本次未交付该用途（例如未要求轮换的普通更新）：保持既有行为，
			// 由上游的用途完整性校验负责拒绝真正缺失的凭据。
			continue
		}
		if values[pair.idKey] != credential.OAuthClient.ClientID || values[pair.secretKey] != credential.PlaintextSecret {
			return serviceCredentialRetryError()
		}
	}
	return nil
}

func booleanEnvironmentValue(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// portalRuntimeSecrets 保留已有材料并补齐各用途独立密钥，DSN沿用同一数据库密码。
// 参数：sourcePath为受控模板或现行运行文件，不处理浏览器与机器OAuth凭据。
// 返回值：可原子写入的配置；随机源或文件错误时失败，不轮换已有密钥。
func portalRuntimeSecrets(sourcePath string) (map[string]string, error) {
	values, err := readEnvironmentValues(sourcePath)
	if err != nil {
		return nil, err
	}
	databasePassword, err := existingOrRandomHex(values["PORTAL_MYSQL_PASSWORD"])
	if err != nil {
		return nil, err
	}
	rootPassword, err := existingOrRandomHex(values["PORTAL_MYSQL_ROOT_PASSWORD"])
	if err != nil {
		return nil, err
	}
	result := map[string]string{
		"PORTAL_MYSQL_PASSWORD":      databasePassword,
		"PORTAL_MYSQL_ROOT_PASSWORD": rootPassword,
		"PORTAL_MYSQL_DSN":           "portal:" + databasePassword + "@tcp(portal-mysql:3306)/customer_portal?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true",
	}
	for _, key := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64", "PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"} {
		value := strings.TrimSpace(values[key])
		if value == "" || strings.HasPrefix(value, "REPLACE_WITH_") {
			buffer := make([]byte, 32)
			if _, err := rand.Read(buffer); err != nil {
				return nil, fmt.Errorf("generate Portal key: %w", err)
			}
			value = base64.StdEncoding.EncodeToString(buffer)
		}
		result[key] = value
	}
	return result, nil
}

func existingOrRandomHex(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !strings.HasPrefix(value, "REPLACE_WITH_") {
		return value, nil
	}
	return randomHex(32)
}

func readEnvironmentValues(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !validEnvironmentKey(strings.TrimSpace(key)) {
			continue
		}
		result[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// maybeSyncContractCatalogLocked 为配置的目标子系统运行平台的目录同步辅助镜像。helper 从
// 平台 MySQL 取出应用自有的角色/权限清单，铸造目录发布者访问令牌，再用子系统自身的服务
// 凭据把清单 PUT 回平台的 /authorization-catalog 端点。
//
// helper 现在经 bash 直接运行在 Agent 主机上（SEC-F2）：此前的一次性
// `docker run --rm --network=host -v /var/run/docker.sock` 会把等价 root 的 Docker socket
// 连同明文 secret 交给临时容器。同步失败不是致命错误：运维可以在带外重跑脚本，常规手册
// 流程仍然可用。
func (provisioner *LocalDockerSubsystemProvisioner) maybeSyncContractCatalogLocked(operationCtx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.config.CatalogSyncEnabled {
		return nil
	}
	if strings.TrimSpace(provisioner.config.CatalogSyncTargetAppCode) != "" &&
		input.ApplicationCode != provisioner.config.CatalogSyncTargetAppCode {
		return nil
	}
	if strings.TrimSpace(input.CatalogPublisherClientID) == "" || strings.TrimSpace(input.CatalogPublisherClientSecret) == "" {
		return fmt.Errorf("catalog publisher client credentials are missing for application %s", input.ApplicationCode)
	}
	if strings.TrimSpace(provisioner.config.CatalogSyncMysqlContainer) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlUser) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlPassword) == "" {
		return fmt.Errorf("catalog sync MySQL coordinates are not fully configured")
	}
	// 安全（SEC-F2）：不再以 docker run --rm --network=host -v /var/run/docker.sock 运行 helper——
	// docker.sock 等价宿主机 root，与 host 网络和注入的明文密钥组合后，helper 容器被攻破即失陷
	// 宿主机。改为由 Agent 自身执行受审脚本：无容器、无挂载、无 host 网络，密钥只进入 Agent
	// 子进程环境，既不进入任何容器，也不出现在命令行参数里（MySQL 访问由脚本按需自行发起）。
	scriptPath := filepath.Join(filepath.Dir(provisioner.config.GatewayScriptPath), "sync-contract-catalog.sh")
	if info, statErr := os.Stat(scriptPath); statErr != nil || info.IsDir() {
		return fmt.Errorf("catalog sync script is unavailable")
	}
	runnerEnvironment := append(os.Environ(),
		"PLATFORM_APPLICATION_ID="+input.ApplicationID,
		"PLATFORM_BASE_URL="+input.Issuer,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID="+input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET="+input.CatalogPublisherClientSecret,
		"PLATFORM_MYSQL_CONTAINER="+provisioner.config.CatalogSyncMysqlContainer,
		"PLATFORM_MYSQL_USER="+provisioner.config.CatalogSyncMysqlUser,
		"PLATFORM_MYSQL_PASSWORD="+provisioner.config.CatalogSyncMysqlPassword,
		"PLATFORM_MYSQL_DATABASE="+provisioner.config.CatalogSyncMysqlDatabase,
	)
	return provisioner.runner.Run(operationCtx, filepath.Dir(scriptPath), runnerEnvironment, "/bin/bash", scriptPath)
}

func (provisioner *LocalDockerSubsystemProvisioner) frontendContainerID(ctx context.Context, directory string) (string, error) {
	command := exec.CommandContext(ctx, provisioner.config.DockerBinary, "ps",
		"--filter", "label=com.docker.compose.project="+provisioner.config.PlatformComposeProject,
		"--filter", "label=com.docker.compose.service="+provisioner.config.PlatformFrontendService,
		"--format", "{{.ID}}")
	command.Dir = directory
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return "", provisioningError("locate portal frontend container")
	}
	identifiers := strings.Fields(string(output))
	if len(identifiers) != 1 {
		return "", provisioningError("portal frontend container is not uniquely available")
	}
	return identifiers[0], nil
}

func (provisioner *LocalDockerSubsystemProvisioner) projectDirectory(applicationCode string) (string, error) {
	applicationCode = strings.TrimSpace(applicationCode)
	if !subsystemDirectoryCodePattern.MatchString(applicationCode) || strings.TrimSpace(provisioner.config.ProjectsRoot) == "" {
		return "", provisioningError("subsystem project path is invalid")
	}
	root, err := filepath.Abs(provisioner.config.ProjectsRoot)
	if err != nil {
		return "", provisioningError("subsystem projects root is invalid")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", provisioningError("subsystem projects root is unavailable")
	}
	directoryCode := applicationCode
	if applicationCode == integratedPortalApplicationCode {
		directoryCode = integratedCustomerApplicationCode
	} else if applicationCode == integratedSettlementApplicationCode {
		directoryCode = "Settlement"
	}
	candidate := filepath.Join(root, directoryCode)
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", provisioningError("subsystem project directory is unavailable")
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", provisioningError("subsystem project directory is outside the configured root")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", provisioningError("subsystem project directory is unavailable")
	}
	return candidate, nil
}

func locateComposeFile(projectDirectory string) (string, error) {
	for _, name := range []string{"docker-compose.yml", "compose.yaml", "compose.yml", "docker-compose.yaml"} {
		candidate := filepath.Join(projectDirectory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func locateEnvironmentSource(projectDirectory string) (string, error) {
	for _, name := range []string{".env.local", ".env.example"} {
		candidate := filepath.Join(projectDirectory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

// ensureContractSigningPhoneEnvironmentValue（SEC-D7b）确保合同签署人手机号列加密密钥
// （SIGNING_PHONE_ENCRYPTION_KEY_BASE64，AES-GCM 32 字节 Base64）包含在本次写入的 values 中：
// contract env 可能由模板 .env.example 首次生成（占位/空值），缺配时 contract /readyz 与
// 寄送写入失败关闭。已有合法键绝不覆盖——轮换会导致既有密文无法解密，需另行数据迁移。
func ensureContractSigningPhoneEnvironmentValue(environmentPath string, values map[string]string) error {
	const key = "SIGNING_PHONE_ENCRYPTION_KEY_BASE64"
	if current, err := readEnvironmentValues(environmentPath); err == nil {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(current[key])); decodeErr == nil && len(decoded) == 32 {
			return nil
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return provisioningError("generate contract signing phone encryption key")
	}
	values[key] = base64.StdEncoding.EncodeToString(secret)
	return nil
}

func updateSubsystemEnvironment(sourcePath, destinationPath string, replacements map[string]string) error {
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	for key, value := range replacements {
		if !validEnvironmentValue(value) || !validEnvironmentKey(key) {
			return errors.New("invalid environment replacement")
		}
	}

	remaining := make(map[string]string, len(replacements))
	for key, value := range replacements {
		remaining[key] = value
	}
	lines := make([]string, 0, strings.Count(string(content), "\n")+len(replacements)+1)
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := scanner.Text()
		key, ok := environmentLineKey(line)
		if replacement, exists := remaining[key]; ok && exists {
			lines = append(lines, key+"="+encodeEnvironmentValue(replacement))
			delete(remaining, key)
			continue
		}
		if ok && strings.Contains(strings.ToUpper(key), "PASSWORD") {
			_, current, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export ")), "=")
			if found && strings.HasPrefix(strings.Trim(strings.TrimSpace(current), "\"'"), "REPLACE_WITH_") {
				generated, generateErr := randomHex(32)
				if generateErr != nil {
					return generateErr
				}
				lines = append(lines, key+"="+generated)
				continue
			}
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, key := range sortedEnvironmentKeys(remaining) {
		lines = append(lines, key+"="+encodeEnvironmentValue(remaining[key]))
	}
	output := strings.Join(lines, "\n") + "\n"

	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destinationPath), ".env.local.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.WriteString(output)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporaryPath, destinationPath)
}

// updateProductionSubsystemEnvironment 只更新显式白名单键。它不沿用本地模板的密码自动生成
// 逻辑，并保留原文件所有者，防止 root Agent 原子替换后让低权限发布账号失去读取权限。
func updateProductionSubsystemEnvironment(path string, replacements map[string]string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("production environment file is unavailable")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for key, value := range replacements {
		if !validEnvironmentKey(key) || !validEnvironmentValue(value) {
			return errors.New("invalid production environment replacement")
		}
	}

	remaining := make(map[string]string, len(replacements))
	for key, value := range replacements {
		remaining[key] = value
	}
	seenManaged := make(map[string]struct{}, len(replacements))
	lines := make([]string, 0, strings.Count(string(content), "\n")+len(replacements)+1)
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := scanner.Text()
		key, ok := environmentLineKey(line)
		if replacement, managed := replacements[key]; managed && ok {
			if _, duplicate := seenManaged[key]; duplicate {
				return fmt.Errorf("duplicate managed production environment key %s", key)
			}
			seenManaged[key] = struct{}{}
			lines = append(lines, key+"="+encodeEnvironmentValue(replacement))
			delete(remaining, key)
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, key := range sortedEnvironmentKeys(remaining) {
		lines = append(lines, key+"="+encodeEnvironmentValue(remaining[key]))
	}
	output := strings.Join(lines, "\n") + "\n"

	temporary, err := os.CreateTemp(filepath.Dir(path), ".production-env.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.WriteString(output)
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
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(temporaryPath, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func environmentLineKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}
	key, _, found := strings.Cut(trimmed, "=")
	key = strings.TrimSpace(key)
	return key, found && validEnvironmentKey(key)
}

func validEnvironmentKey(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validEnvironmentValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}

func encodeEnvironmentValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return !(character == '-' || character == '_' || character == '.' || character == '/' || character == ':' || character == '@' || character == ',' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9')
	}) == -1 {
		return value
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func randomHex(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func sortedEnvironmentKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func provisioningError(message string) error {
	return fmt.Errorf("%w: %s", application.ErrSubsystemProvisioningUnavailable, message)
}

// localReleaseCredentialError 用于“已批准本地发行缺少组件凭据”。它同时暴露两个哨兵：
// ErrSubsystemProvisioningUnavailable 维持既有错误处理路径，ErrRuntimeLicenseNotInitialized
// 让上层把失败归类为 LICENSE_NOT_INITIALIZED，向用户给出可执行的许可证指引。
type localReleaseCredentialError struct{ message string }

// Error 返回构造时固化的描述文本。
func (e localReleaseCredentialError) Error() string { return e.message }

// Unwrap 同时暴露两个哨兵：errors.Is 据此既能匹配既有部署不可用路径，也能把失败归类为
// LICENSE_NOT_INITIALIZED，两条处理链都不需要解析错误文本。
func (e localReleaseCredentialError) Unwrap() []error {
	return []error{application.ErrSubsystemProvisioningUnavailable, application.ErrRuntimeLicenseNotInitialized}
}

// localRuntimeDeploymentError 把重建失败收口为稳定的部署不可用错误，同时保留下层原因：
// 只留外层短语会让运维在状态表里只看到“部署 Agent 执行失败”，无法判断是许可运行态、
// 镜像还是数据库。下层摘要已在抓取处截断并脱敏，这里再压成单行并限长。
func localRuntimeDeploymentError(err error, fallback string) error {
	if application.RuntimeLicenseApprovalErrorCode(err) != "" {
		return application.RuntimeLicenseApprovalFailure(err)
	}
	// 下层原因必须随外层短语一起返回：只留短语会让状态表只剩“部署 Agent 执行失败”，
	// 运维看不到“commercial license configuration failed”这类真实根因。输出先压成
	// 单行、限长，避免破坏 last_error_message 的 varchar(1000) 与 JSON。
	detail := singleLineFailureDetail(err.Error(), nil)
	if detail == "" {
		return provisioningError(fallback)
	}
	return fmt.Errorf("%w: %s: %s", application.ErrSubsystemProvisioningUnavailable, fallback, detail)
}

// ApplyAccess 重写本机访问覆盖并重建现行固定集合中的平台、合同与 CRM 容器。
// 参数：ctx 控制期限，input 提供经校验的公开 origin 与 HTTP 重定向策略。
// 返回值：无；运行配置、输入校验、覆盖写入或重建失败时返回错误。
func (provisioner *LocalDockerSubsystemProvisioner) ApplyAccess(ctx context.Context, input settingsapplication.AccessApplyInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()

	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, err := provisioner.integratedComposeConfiguration("up", "contract-api", "customer-api")
	if err != nil {
		return err
	}

	overrideFile := filepath.Join(platformRoot, "docker", ".env.lan")
	customerOverrideFile := filepath.Join(platformRoot, "docker", ".env.customer.lan")
	placeholderFile := filepath.Join(platformRoot, "docker", ".env.lan.disabled")
	if info, statErr := os.Stat(placeholderFile); statErr != nil || info.IsDir() {
		return provisioningError("access placeholder environment file is unavailable")
	}

	// 安全（SEC-F1）：origin 会以 "KEY=origin" 逐行写入 .env.lan 并注入容器环境，使用前必须
	// 拒绝 CR/LF/NUL 换行注入、非 http(s) scheme 以及携带 path/query/fragment/userinfo 的值。
	origin, err := normalizeAccessPublicOrigin(input.PublicOrigin)
	if err != nil {
		return err
	}
	port, err := accessHTTPPort(origin)
	if err != nil {
		return err
	}
	if origin == "" {
		_ = os.Remove(overrideFile)
		_ = os.Remove(customerOverrideFile)
		return provisioner.runAccessCompose(ctx, platformRoot, workspaceRoot, composeFile,
			platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment,
			placeholderFile, placeholderFile, "127.0.0.1", port,
			"up", "-d", "--no-deps", "--wait", "api", "contract-api", "contract-worker", "customer-api", "frontend")
	}

	if err := writeAccessOverrideFiles(overrideFile, customerOverrideFile, origin, input.AllowInsecureHTTPRedirect); err != nil {
		return err
	}
	return provisioner.runAccessCompose(ctx, platformRoot, workspaceRoot, composeFile,
		platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment,
		overrideFile, customerOverrideFile, "0.0.0.0", port,
		"up", "-d", "--no-deps", "--wait", "api", "contract-api", "contract-worker", "customer-api", "frontend")
}

func (provisioner *LocalDockerSubsystemProvisioner) runAccessCompose(ctx context.Context, platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, lanOverride, customerOverride, bindAddress, port string, arguments ...string) error {
	composeArguments := []string{
		"compose", "--project-name", provisioner.config.PlatformComposeProject,
		"--project-directory", platformRoot,
		"--env-file", platformEnvironment,
		"--env-file", contractEnvironment,
		"--env-file", customerEnvironment,
		"--env-file", portalEnvironment,
		"--env-file", projectEnvironment,
		"-f", composeFile,
	}
	composeArguments = append(composeArguments, arguments...)
	runnerEnvironment := append([]string{}, os.Environ()...)
	runnerEnvironment = append(runnerEnvironment,
		"BASIC_PLATFORM_RUNTIME_ENV_FILE="+platformEnvironment,
		"CONTRACT_RUNTIME_ENV_FILE="+contractEnvironment,
		"CUSTOMER_RUNTIME_ENV_FILE="+customerEnvironment,
		"BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE="+lanOverride,
		"CONTRACT_LAN_OVERRIDE_ENV_FILE="+lanOverride,
		"CUSTOMER_LAN_OVERRIDE_ENV_FILE="+customerOverride,
		"PORTAL_RUNTIME_ENV_FILE="+portalEnvironment,
		"PORTAL_LAN_OVERRIDE_ENV_FILE="+filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
		"PROJECT_RUNTIME_ENV_FILE="+projectEnvironment,
		"PROJECT_LAN_OVERRIDE_ENV_FILE="+lanOverride,
		"BASIC_PLATFORM_HOST_PROJECT_ROOT="+platformRoot,
		"SUBSYSTEM_HOST_PROJECTS_ROOT="+workspaceRoot,
		"FRONTEND_BIND_ADDRESS="+bindAddress,
		"FRONTEND_HTTP_PORT="+port,
	)
	return provisioner.runner.Run(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
}

func writeAccessOverrideFiles(overrideFile, customerOverrideFile, origin string, allowInsecureHTTP bool) error {
	if err := writeAccessOverrideFile(overrideFile, "docker/.env.lan",
		"APP_PUBLIC_BASE_URL="+origin+"\n"+
			"APP_CORS_ALLOWED_ORIGINS="+origin+"\n"+
			"OIDC_ISSUER="+origin+"\n"+
			"OIDC_ISSUER_BASE_URL="+origin+"\n"+
			"AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS="+strconv.FormatBool(allowInsecureHTTP)+"\n"+
			"OIDC_REDIRECT_URI="+origin+"/contract_management/auth/callback\n"+
			"APP_PUBLIC_URL="+origin+"/contract_management/\n"); err != nil {
		return err
	}
	return writeAccessOverrideFile(customerOverrideFile, "docker/.env.customer.lan",
		"APP_PUBLIC_ORIGIN="+origin+"\n"+
			"OIDC_ISSUER="+origin+"\n"+
			"OIDC_REDIRECT_URI="+origin+"/customer-opportunity/auth/callback\n"+
			"OIDC_POST_LOGOUT_REDIRECT_URI="+origin+"/customer-opportunity/\n")
}

func writeAccessOverrideFile(path, description, content string) error {
	// 安全（SEC-F1）：content 会被 Compose 当作 KEY=value 环境文件加载，落盘前逐行强制环境值
	// 校验——拒绝 \r/\x00 等控制字符并要求每行都是合法 KEY=value，防止换行注入覆盖其他容器
	// 环境（如 DEV_AUTH_ENABLED、OIDC_ISSUER）；换行只允许作为行分隔符出现。
	for _, line := range strings.Split(content, "\n") {
		if line == "" {
			continue
		}
		if !validEnvironmentValue(line) {
			return provisioningError("write " + description + ": environment line contains control characters")
		}
		if key, _, found := strings.Cut(line, "="); !found || !validEnvironmentKey(key) {
			return provisioningError("write " + description + ": environment line must be KEY=value")
		}
	}
	payload := "# 由平台「对外访问」配置自动生成；执行恢复本机访问会删除本文件。\n# 仅用于临时访问，不要提交到版本库。\n" + content
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return provisioningError("write " + description + ": " + err.Error())
	}
	return os.Chmod(path, 0o600)
}

// accessHTTPPort 提取 origin 的显式端口。安全（SEC-F1）：URL 解析或 scheme 校验失败必须返回
// 错误而不是静默回退默认端口，避免非法 origin 被当作合法配置继续写入运行时环境。
func accessHTTPPort(origin string) (string, error) {
	if origin == "" {
		// 空 origin 表示恢复本机访问，沿用固定默认端口。
		return "8081", nil
	}
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		// 不把 url.Parse 的错误文本带回上层：它可能携带原始输入，避免日志二次注入。
		return "", provisioningError("public origin is not a valid http(s) origin")
	}
	if parsed.Port() == "" {
		return "8081", nil
	}
	return parsed.Port(), nil
}
