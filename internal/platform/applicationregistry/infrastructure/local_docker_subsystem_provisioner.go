package infrastructure

import (
	"bufio"
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
	integratedCustomerRoleConfigHash = "sha256:1c1d94091fdfd3f5665d04215b91089434a737b0f43e9f1a9901755af97f621a"
	integratedPortalRoleConfigHash   = "sha256:95f1d1283d3a251e9b9167aa291c19bf14d265105ccedfc661aca096666a37b8"
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

// RunOutput 与 Run 等价，但额外把命令合并输出返回给调用方。生产 Agent 在固定部署步骤
// 失败时会用它在受限时间内抓取目标容器日志摘要，把真实原因附到 provisioning 错误里，
// 让平台页面直接看到而不是只显示通用的健康检查提示。输出仍先截断再写入 Agent 标准错误。
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
	output, err := command.CombinedOutput()
	if err != nil {
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
	return &LocalDockerSubsystemProvisioner{config: config, runner: runner}, nil
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

// Provision 应用生成配置并执行部署；所有调用串行化，因为一次运行会同时修改共享 Docker、
// 环境文件和 nginx 状态，失败后应沿用部署状态重试而非并发补偿。
func (provisioner *LocalDockerSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	// 所有接入/更新/下线共享同一把进程锁，因为它们会改写公共 Compose 与 nginx 配置；
	// 并发执行可能让一个请求覆盖另一个请求刚写好的密钥或网关片段。
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.applyLocked(ctx, input)
}

// Update 重建运行中的子系统容器，但不改动 .env.local 与门户网关。适用场景是“子系统代码
// 变更后重新部署”；BaseURL/UpstreamURL/secret 变更应先通过常规管理端点 PATCH 环境与
// OAuth 客户端，再单独执行更新流程。让 Update 在集成层保持无副作用，是为了规避 bcrypt
// 哈希后的客户端 secret 无法还原、进而无法重新签发 .env.local 的问题。
func (provisioner *LocalDockerSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.rebuildLocked(ctx, input)
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
			if applicationCode == integratedCustomerApplicationCode {
				services = []string{"customer-api"}
			} else if applicationCode == integratedPortalApplicationCode {
				services = []string{"portal-api", "portal-report-worker"}
			}
			if applicationCode == integratedProjectApplicationCode {services=[]string{"project-api","sla-notifier"}}
			if localApprovalErr == nil && len(localLicenseServices[applicationCode]) > 0 {
				services = append([]string(nil), localLicenseServices[applicationCode]...)
			}
            var background *localProjectBackground
            if applicationCode==integratedProjectApplicationCode {
                var err error
                background,err=provisioner.projectBackgroundTarget(operationCtx);if err!=nil{return err}
                if err:=prepareProjectBackground(operationCtx,background,true);err!=nil{return err}
            }
			if runErr := provisioner.runIntegratedPlatformCompose(operationCtx, append([]string{"stop"}, services...)...); runErr != nil {
				return provisioningError("stop subsystem containers")
			}
            if background!=nil {if err:=markProjectBackgroundStopped(background);err!=nil{return err}}
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

// rebuildLocked 重建子系统 Compose 栈且不改网关。管理页同时提供当前网关字段时，也会更新
// 非密钥的公开运行时值，同时保留磁盘上已有的一次性凭据。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if err := provisioner.deliverLocalRuntimeLicense(ctx, input); err != nil {
		return err
	}

	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
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
	case input.ApplicationCode == integratedProjectApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedProjectStack(operationCtx)
	default:
		composeFile, locateErr := locateComposeFile(projectDirectory)
		if locateErr != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
		rebuildErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
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
	case "data_analysis":
		contractCredential, ok := input.ServiceCredential(application.ServiceCredentialContractDashboardRead)
		if !ok {
			return provisioningError("data analysis contract dashboard credential is unavailable")
		}
		projectCredential, ok := input.ServiceCredential(application.ServiceCredentialProjectDashboardRead)
		if !ok {
			return provisioningError("data analysis project dashboard credential is unavailable")
		}
		expectedContractClientID := input.ApplicationCode + "-" + input.Environment + "-contract-dashboard"
		if contractCredential.OAuthClient.ClientID != expectedContractClientID {
			return provisioningError("data analysis contract dashboard credential does not match the target environment")
		}
		expectedProjectClientID := input.ApplicationCode + "-" + input.Environment + "-project-dashboard"
		if projectCredential.OAuthClient.ClientID != expectedProjectClientID {
			return provisioningError("data analysis project dashboard credential does not match the target environment")
		}
		values["MACHINE_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
		values["MACHINE_TOKEN_ISSUER"] = "basic-platform"
		values["MACHINE_TOKEN_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_MACHINE_CLIENT_ID"] = contractCredential.OAuthClient.ClientID
		values["CONTRACT_MACHINE_CLIENT_SECRET"] = contractCredential.PlaintextSecret
		values["CONTRACT_MACHINE_TOKEN_SCOPE"] = "dashboard.contract.read"
		values["PROJECT_MACHINE_CLIENT_ID"] = projectCredential.OAuthClient.ClientID
		values["PROJECT_MACHINE_CLIENT_SECRET"] = projectCredential.PlaintextSecret
		values["PROJECT_MACHINE_TOKEN_SCOPE"] = "dashboard.project.read"
	}
	if err := provisioner.p1LocalRuntimeValues(input, environmentPath, values, false); err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem service credential configuration")
	}
	return nil
}

func (provisioner *LocalDockerSubsystemProvisioner) updatePublicRuntimeConfiguration(input application.SubsystemProvisioningInput, environmentPath string) error {
	publicOrigin, err := publicOriginFromURL(input.PublicURL)
	if err != nil {
		return provisioningError("subsystem public URL is invalid")
	}
	secure := booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(publicOrigin), "https://"))
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
		values["OIDC_ROLE_CONFIG_HASH"] = integratedCustomerRoleConfigHash
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		values["PORTAL_ROLE_CONFIG_HASH"] = integratedPortalRoleConfigHash
	}
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem public runtime configuration")
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
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

func (provisioner *LocalDockerSubsystemProvisioner) updateOIDCRuntimeConfiguration(input application.SubsystemProvisioningInput, environmentPath string) error {
	values := map[string]string{}
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
		values["PORTAL_ROLE_CONFIG_HASH"] = integratedPortalRoleConfigHash
	} else {
		values["OIDC_ISSUER"] = input.Issuer
		if backchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = backchannel
		}
		if input.ApplicationCode == integratedCustomerApplicationCode {
			values["OIDC_ROLE_CONFIG_HASH"] = integratedCustomerRoleConfigHash
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

// applyLocked 包含 Provision 的实际写入与启动流程，调用方必须先持有互斥锁；环境文件采用
// 临时文件加原子替换，Compose 或网关失败时保留现场，便于诊断和安全重试。
func (provisioner *LocalDockerSubsystemProvisioner) applyLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if err := provisioner.deliverLocalRuntimeLicense(ctx, input); err != nil {
		return err
	}

	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
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
		values["OIDC_ROLE_CONFIG_HASH"] = integratedCustomerRoleConfigHash
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
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		credentials, credentialErr := requiredPortalServiceCredentials(input)
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
			"PORTAL_ROLE_CONFIG_HASH":                     integratedPortalRoleConfigHash,
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
		if err := updateSubsystemEnvironment(customerEnvironment, customerEnvironment, customerValues); err != nil {
			return provisioningError("write CRM Portal integration configuration")
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
		platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
		customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
		customerValues := map[string]string{
			"CONTRACT_VERIFICATION_ENABLED":   "true",
			"CONTRACT_SUMMARY_URL":            "http://contract-api:8081/internal/contract-summary",
			"CONTRACT_SUMMARY_TOKEN_URL":      "http://platform-api:8080/oauth2/token",
			"CONTRACT_SUMMARY_CLIENT_ID":      credentials[application.ServiceCredentialContractSummaryRead].OAuthClient.ClientID,
			"CONTRACT_SUMMARY_CLIENT_SECRET":  credentials[application.ServiceCredentialContractSummaryRead].PlaintextSecret,
			"CONTRACT_SUMMARY_SCOPE":          "contract.summary.read",
			"CONTRACT_TOKEN_URL":              "http://platform-api:8080/oauth2/token",
			"CONTRACT_OPPORTUNITY_INTAKE_URL": "http://contract-api:8081/internal/opportunity-signed-events",
			"CONTRACT_CLIENT_ID":              credentials[application.ServiceCredentialContractOpportunitySignedWrite].OAuthClient.ClientID,
			"CONTRACT_CLIENT_SECRET":          credentials[application.ServiceCredentialContractOpportunitySignedWrite].PlaintextSecret,
			"CONTRACT_SCOPE":                  "opportunity.signed.write",
		}
		if err := updateSubsystemEnvironment(customerEnvironment, customerEnvironment, customerValues); err != nil {
			return provisioningError("write CRM contract integration configuration")
		}
	}
	if err := provisioner.p1LocalRuntimeValues(input, environmentPath, values, true); err != nil {
		return err
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

// rebuildIntegratedPortalStack 复用 customer 仓库的不可变后端镜像，同时使用独立的 Portal
// 数据库与进程。目录在 Portal 启动前发布，随后重建 customer-api，让邀请端点拿到生成的
// 最小权限机器凭据。
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedPortalStack(ctx context.Context) error {
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
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--force-recreate", "--no-deps", "customer-api")
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformCompose(ctx context.Context, arguments ...string) error {
	_, err := provisioner.executeIntegratedPlatformCompose(ctx, false, arguments...)
	return err
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformComposeOutput(ctx context.Context, arguments ...string) ([]byte, error) {
	return provisioner.executeIntegratedPlatformCompose(ctx, true, arguments...)
}

func (provisioner *LocalDockerSubsystemProvisioner) executeIntegratedPlatformCompose(ctx context.Context, capture bool, arguments ...string) ([]byte, error) {
	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, err := provisioner.integratedComposeConfiguration()
	if err != nil {
		return nil, err
	}
	settlementEnvironment := filepath.Join(workspaceRoot, "Settlement", ".env.local")
	if info, statErr := os.Stat(settlementEnvironment); statErr != nil || info.IsDir() {
		// 其他集成子系统测试与旧工作区可能还没有 Settlement。Compose 解析 anchor 时仍需要
		// 一个 env-file 目标，因此非 Settlement 操作一律使用空的安全文件。
		settlementEnvironment = os.DevNull
	}
	// 集成子系统重建必须确定性执行，不得继承启动平台 API 的 shell 中残留的 LAN 覆盖值。
	// 常规 docker-local 入口会显式设置这些值；provisioner 从 API 容器内部运行 Compose，必须
	// 做同样的事。否则 Compose 可能解析到旧的或缺失的 platform/docker/.env.lan，把
	// contract-api 卡在 Created 状态。
	lanPlaceholder := filepath.Join(platformRoot, "docker", ".env.lan.disabled")
	analysisEnvironment := filepath.Join(workspaceRoot, "data_analysis", ".env.local")
	if info, err := os.Stat(analysisEnvironment); err != nil || info.IsDir() {
		analysisEnvironment = os.DevNull
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
	}
	workerEnvironment := filepath.Join(filepath.Dir(portalEnvironment), ".env.portal-report-worker.local")
	if info, err := os.Stat(workerEnvironment); err != nil || info.IsDir() {
		workerEnvironment = os.DevNull
	}
	runnerEnvironment := make([]string, 0, len(os.Environ())+12)
	runnerEnvironment = append(runnerEnvironment, "PORTAL_REPORT_WORKER_RUNTIME_ENV_FILE="+workerEnvironment)
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
		"DATA_ANALYSIS_RUNTIME_ENV_FILE="+analysisEnvironment,
		"PRESALE_WORKER_ENV_FILE="+filepath.Join(workspaceRoot, "customer_and_opportunity", ".env.presale-worker"),
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
	return nil, provisioner.runner.Run(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
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

func (provisioner *LocalDockerSubsystemProvisioner) integratedComposeConfiguration() (string, string, string, string, string, string, string, string, error) {
	platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
	workspaceRoot := filepath.Dir(platformRoot)
	composeFile := provisioner.localComposeFile()
	platformEnvironment := filepath.Join(platformRoot, "docker", ".env.local")
	contractEnvironment := filepath.Join(workspaceRoot, integratedContractApplicationCode, ".env.local")
	customerEnvironment := filepath.Join(platformRoot, "docker", ".env.customer.local")
	portalEnvironment := filepath.Join(platformRoot, "docker", ".env.portal.local")
	projectEnvironment := filepath.Join(workspaceRoot, integratedProjectApplicationCode, ".env.local")
	for _, required := range []string{composeFile, platformEnvironment, contractEnvironment, customerEnvironment} {
		if info, err := os.Stat(required); err != nil || info.IsDir() {
			return "", "", "", "", "", "", "", "", provisioningError("integrated subsystem Compose configuration is unavailable")
		}
	}
	// 既有本地安装早于 customer_portal。其他集成子系统必须保持可用，不能强迫立即生成新的
	// 环境文件。Portal preflight 单独检查自身模板，且 Provision 会在运行 Compose 之前写好
	// .env.portal.local。
	if info, err := os.Stat(portalEnvironment); err != nil || info.IsDir() {
		portalEnvironment = os.DevNull
	}
	// 既有本地安装早于 project_management。项目环境文件由 Provision 在启动集成栈之前写入；
	// 缺失文件按 Portal 同样处理，让既有工作区在首次接入前保持可用。
	if info, err := os.Stat(projectEnvironment); err != nil || info.IsDir() {
		projectEnvironment = os.DevNull
	}
	return platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, nil
}

func (provisioner *LocalDockerSubsystemProvisioner) subsystemEnvironmentPaths(applicationCode, projectDirectory string) (string, string, error) {
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

func isIntegratedSubsystem(applicationCode string) bool {
	switch strings.TrimSpace(applicationCode) {
	case integratedContractApplicationCode, integratedCustomerApplicationCode, integratedPortalApplicationCode, integratedProjectApplicationCode, integratedSettlementApplicationCode:
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

func booleanEnvironmentValue(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// portalRuntimeSecrets 保留已生成的本地材料，并在首次 Agent 驱动接入时补齐全部 secret。
// DSN 始终由同一个生成的数据库密码重建，避免连接串中出现模板占位符或第二个不相关的随机
// 值。浏览器与机器 OAuth 凭据有意交由调用方处理。
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
	for _, key := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64"} {
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

func localRuntimeDeploymentError(err error, fallback string) error {
	if application.RuntimeLicenseApprovalErrorCode(err) != "" {
		return application.RuntimeLicenseApprovalFailure(err)
	}
	return provisioningError(fallback)
}

// ApplyAccess 重写本机覆盖环境文件并重建受影响容器，使统一前端、OIDC issuer 和子系统回调
// 使用配置的公开 origin；空的 PublicOrigin 恢复仅本机访问（127.0.0.1）。行为对齐
// scripts/lan-access.sh，但由平台管理控制台经 socket 驱动。
func (provisioner *LocalDockerSubsystemProvisioner) ApplyAccess(ctx context.Context, input settingsapplication.AccessApplyInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()

	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, err := provisioner.integratedComposeConfiguration()
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
