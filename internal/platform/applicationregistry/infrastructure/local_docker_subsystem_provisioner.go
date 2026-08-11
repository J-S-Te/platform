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
	integratedContractApplicationCode = "contract_management"
	integratedCustomerApplicationCode = "customer_and_opportunity"
	integratedPortalApplicationCode   = "customer_portal"
	integratedProjectApplicationCode  = "project_management"
	// This is the compatibility hash compiled into the customer authorization catalog. The
	// customer's catalog tests deliberately fail when its role mapping changes, forcing this
	// deployment contract to be updated in the same reviewed release.
	integratedCustomerRoleConfigHash = "sha256:609436cbb09101c385c572ae41714202a83511b42cc675b7aa56e98d1dad536a"
	integratedPortalRoleConfigHash   = "sha256:f67121b52d6d850e99d1c4520d661fb85e26512c7ee50cd83182a8dc39b368d4"
)

// LocalDockerSubsystemProvisionerConfig controls the trusted local Docker automation used by the
// one-click onboarding endpoint. Paths are operator configuration, never browser input.
type LocalDockerSubsystemProvisionerConfig struct {
	Enabled                 bool
	ProjectsRoot            string
	GatewayScriptPath       string
	GatewayIncludePath      string
	PlatformComposeProject  string
	PlatformFrontendService string
	PlatformDockerNetwork   string
	DockerBinary            string
	Timeout                 time.Duration
	// CatalogSync configures the post-onboarding authorization catalog sync hook. When non-empty
	// the provisioner runs the contract_management catalog sync image after the subsystem Compose
	// stack is up, so the platform's authorization catalog reflects the subsystem's role and
	// permission declarations without any in-band code change in the subsystem itself.
	CatalogSyncEnabled        bool
	CatalogSyncImage          string
	CatalogSyncMysqlContainer string
	CatalogSyncMysqlUser      string
	CatalogSyncMysqlPassword  string
	CatalogSyncMysqlDatabase  string
	CatalogSyncTargetAppCode  string
	// Keycloak endpoints are supplied to the isolated deployment Agent rather
	// than derived from a browser request. PublicURL is the browser-visible
	// Keycloak base URL, while InternalURL is the Compose-network endpoint used
	// by Go OIDC clients for discovery, JWKS, UserInfo and token exchange.
	KeycloakPublicURL   string
	KeycloakInternalURL string
	KeycloakRealm       string
}

type subsystemCommandRunner interface {
	Run(context.Context, string, []string, string, ...string) error
}

type execSubsystemCommandRunner struct{}

func (execSubsystemCommandRunner) Run(ctx context.Context, directory string, environment []string, name string, arguments ...string) error {
	_, err := execSubsystemCommandRunner{}.RunOutput(ctx, directory, environment, name, arguments...)
	return err
}

// RunOutput 与 Run 等价，但额外把命令合并输出返回给调用方。生产 Agent 在固定部署步骤
// 失败时会用它在受限时间内抓取目标容器日志摘要，把真实原因附到 provisioning 错误里，
// 让平台页面直接看到而不是只显示通用的健康检查提示。输出仍先截断再写入 Agent 标准错误。
func (execSubsystemCommandRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	// `directory` is treated as a hint: command.Dir only accepts real directories, so the
	// caller may pass a unix socket path (e.g. /var/run/docker.sock) for documentation. Fall
	// back to "/" so exec.Command's chdir probe never reports ENOTDIR on a non-directory.
	if info, err := os.Stat(directory); err == nil && info.IsDir() {
		command.Dir = directory
	} else {
		command.Dir = string(filepath.Separator)
	}
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		// Surface a truncated excerpt of the failed command's output to stderr so operators
		// can diagnose provisioning failures. Do not return command arguments or output
		// verbatim: either may contain implementation details. The OAuth secret is never
		// supplied as an argument, but this rule keeps future changes safe.
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

// NewLocalDockerSubsystemProvisioner constructs the local deployment automation adapter.
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

// oidcBackchannelBaseURL returns the private Compose endpoint only when the
// selected issuer is this deployment's Keycloak Realm. The public issuer is
// still retained in OIDC_ISSUER so go-oidc validates the browser-visible iss
// claim, while its HTTP transport reaches Keycloak without a host-network hop.
func (provisioner *LocalDockerSubsystemProvisioner) oidcBackchannelBaseURL(issuer string) string {
	if provisioner == nil || provisioner.config.KeycloakPublicURL == "" ||
		provisioner.config.KeycloakInternalURL == "" || provisioner.config.KeycloakRealm == "" {
		return ""
	}
	publicIssuer := provisioner.config.KeycloakPublicURL + "/realms/" + provisioner.config.KeycloakRealm
	if strings.TrimRight(strings.TrimSpace(issuer), "/") != publicIssuer {
		return ""
	}
	return provisioner.config.KeycloakInternalURL + "/realms/" + provisioner.config.KeycloakRealm
}

// DiscoverSubsystemServices reports Compose containers through the existing Agent transport.
// Discovery is read-only and does not rewrite .env, Compose, or gateway files.
func (provisioner *LocalDockerSubsystemProvisioner) DiscoverSubsystemServices(ctx context.Context, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	if provisioner == nil || !provisioner.config.Enabled {
		return nil, provisioningError("automatic subsystem deployment is disabled")
	}
	return discoverDockerLabelServices(ctx, provisioner.config.DockerBinary, applicationCode, environment)
}

// DiscoverSubsystemCandidates returns only label-declared, unregistered service metadata.  The
// result cannot write files or start containers and is safe to expose to the audited onboarding UI.
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

// Update rebuilds the running subsystem containers without touching .env.local or the portal
// gateway. The use case is "subsystem code changed, redeploy"; for BaseURL/UpstreamURL/secret
// changes the caller is expected to PATCH the environment and OAuth client first via the regular
// management endpoints, then run a separate update flow. Keeping Update side-effect free on the
// integration layer avoids the problem of the bcrypt-hashed client secret not being recoverable
// for a re-issued .env.local.
func (provisioner *LocalDockerSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.rebuildLocked(ctx, input)
}

// Teardown 停止 Compose、删除生成的 .env.local、移除门户网关片段并 reload nginx；它不删除
// 控制面记录，HTTP 层随后负责 DELETE，因此清理失败可单独重试，不会误报数据库已回滚。
func (provisioner *LocalDockerSubsystemProvisioner) Teardown(ctx context.Context, _ /* tenant */ string, applicationCode, _ /* environment */ string) error {
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

	// 集成子系统与平台共用 Compose 项目，因此只能停止自己的 API 服务，不能执行 down；
	// 独立子系统则拥有完整栈和 .env.local，可按项目整体清理。
	// Standalone Compose stacks and their .env.local live under the subsystem project directory.
	// The integrated contract, customer and Portal APIs share the platform Compose project, so teardown
	// only stops their API service and deliberately preserves database/key material in the shared
	// runtime environment files.
	projectDirectory, projectErr := provisioner.projectDirectory(applicationCode)
	if projectErr == nil {
		if isIntegratedSubsystem(applicationCode) {
			service := "contract-api"
			if applicationCode == integratedCustomerApplicationCode {
				service = "customer-api"
			} else if applicationCode == integratedPortalApplicationCode {
				service = "portal-api"
			}
			if runErr := provisioner.runIntegratedPlatformCompose(operationCtx, "stop", service); runErr != nil {
				return provisioningError("stop subsystem containers")
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

	// Best-effort nginx reload. frontendContainerID may fail if the frontend stack is not
	// running; that's fine for the caller.
	if projectDirectory != "" && !isIntegratedSubsystem(applicationCode) {
		if containerID, err := provisioner.frontendContainerID(operationCtx, projectDirectory); err == nil {
			_ = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-t")
			_ = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary, "exec", containerID, "nginx", "-s", "reload")
		}
	}
	return nil
}

// rebuildLocked rebuilds the subsystem Compose stack without modifying the gateway. When the
// management page supplies the current gateway fields, it also updates the non-secret public
// runtime values while preserving the one-time credentials already on disk.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if !provisioner.config.Enabled {
		return provisioningError("automatic subsystem deployment is disabled")
	}
	operationCtx, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()

	projectDirectory, err := provisioner.projectDirectory(input.ApplicationCode)
	if err != nil {
		return err
	}
	_, environmentPath, err := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	if err := provisioner.updateOIDCRuntimeConfiguration(input, environmentPath); err != nil {
		return err
	}
	if strings.TrimSpace(input.PublicURL) != "" {
		if err := provisioner.updatePublicRuntimeConfiguration(input, environmentPath); err != nil {
			return err
		}
	}
	var rebuildErr error
	switch input.ApplicationCode {
	case integratedContractApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case integratedCustomerApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case integratedPortalApplicationCode:
		rebuildErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	default:
		composeFile, locateErr := locateComposeFile(projectDirectory)
		if locateErr != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
		rebuildErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
	}
	if rebuildErr != nil {
		return provisioningError("rebuild subsystem containers")
	}
	// Re-publish the authorization catalog after a controlled rebuild so a subsystem restart
	// that changed its own role/permission set is reflected in the platform. The sync is
	// best-effort: failures are logged but do not abort the update response.
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		fmt.Fprintf(os.Stderr, "[subsystem-provisioner] post-rebuild catalog sync skipped or failed: %v\n", err)
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
			"APP_PATH_PREFIX":               input.PathPrefix,
			"OIDC_REDIRECT_URI":             input.RedirectURI,
			"OIDC_POST_LOGOUT_REDIRECT_URI": input.PublicURL,
			"OIDC_SESSION_COOKIE_SECURE":    secure,
		}
	default:
		values = map[string]string{
			"APP_PUBLIC_URL":             input.PublicURL,
			"APP_PATH_PREFIX":            input.PathPrefix,
			"OIDC_REDIRECT_URI":          input.RedirectURI,
			"OIDC_SESSION_COOKIE_SECURE": secure,
		}
	}
	// A lifecycle update must also re-apply the selected identity provider. The
	// control-plane update endpoint intentionally preserves client secrets, so
	// only the issuer and backchannel are refreshed here.
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
	if input.ApplicationCode == integratedPortalApplicationCode {
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
	if err := updateSubsystemEnvironment(environmentPath, environmentPath, values); err != nil {
		return provisioningError("write subsystem OIDC runtime configuration")
	}
	return nil
}

// applyLocked 包含 Provision 的实际写入与启动流程，调用方必须先持有互斥锁；环境文件采用
// 临时文件加原子替换，Compose 或网关失败时保留现场，便于诊断和安全重试。
func (provisioner *LocalDockerSubsystemProvisioner) applyLocked(ctx context.Context, input application.SubsystemProvisioningInput) error {
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
	if !isIntegratedSubsystem(input.ApplicationCode) {
		composeFile, err = locateComposeFile(projectDirectory)
		if err != nil {
			return provisioningError("subsystem Compose file is unavailable")
		}
	}
	environmentSource, environmentPath, err := provisioner.subsystemEnvironmentPaths(input.ApplicationCode, projectDirectory)
	if err != nil {
		return provisioningError("subsystem environment template is unavailable")
	}
	publicOrigin, publicOriginErr := publicOriginFromURL(input.PublicURL)
	if publicOriginErr != nil {
		return provisioningError("subsystem public URL is invalid")
	}
	values := map[string]string{
		"PLATFORM_BASE_URL":         input.Issuer,
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
		// Local onboarding publishes the catalog through the isolated provisioner after the
		// subsystem starts. Do not couple contract-api availability to this control-plane
		// operation; keep the isolated credential only for explicit reconciliation.
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
		"PLATFORM_DOCKER_NETWORK":                       provisioner.config.PlatformDockerNetwork,
	}
	// 这里构造的 map 是密钥进入运行时文件的唯一通道。后续写文件使用受限权限，命令行
	// 只传 env-file 路径，避免 secret 出现在进程列表或部署日志中。
	if input.ApplicationCode == integratedCustomerApplicationCode {
		// customer-api is compiled into the unified local topology. OIDC still uses the public
		// issuer, while discovery/token calls are routed over the private Compose alias. The
		// compatibility hash is derived from the application's embedded catalog at startup so a
		// catalog-only release cannot drift from a platform-side hard-coded value.
		values["APP_PUBLIC_ORIGIN"] = publicOrigin
		values["DEV_AUTH_ENABLED"] = "false"
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
		values["OIDC_BACKCHANNEL_BASE_URL"] = "http://platform-api:8080"
		if keycloakBackchannel := provisioner.oidcBackchannelBaseURL(input.Issuer); keycloakBackchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = keycloakBackchannel
		}
		values["OIDC_POST_LOGOUT_REDIRECT_URI"] = input.PublicURL
		values["OIDC_ROLE_CONFIG_HASH"] = integratedCustomerRoleConfigHash
		values["OIDC_SESSION_COOKIE_SECURE"] = booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(input.Issuer), "https://"))
		auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		if !ok {
			return provisioningError("customer audit publisher credential is unavailable")
		}
		values["PLATFORM_AUDIT_CLIENT_ID"] = auditCredential.OAuthClient.ClientID
		values["PLATFORM_AUDIT_CLIENT_SECRET"] = auditCredential.PlaintextSecret
	}
	if input.ApplicationCode == integratedProjectApplicationCode {
		values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
		values["OIDC_BACKCHANNEL_BASE_URL"] = "http://platform-api:8080"
		if keycloakBackchannel := provisioner.oidcBackchannelBaseURL(input.Issuer); keycloakBackchannel != "" {
			values["OIDC_BACKCHANNEL_BASE_URL"] = keycloakBackchannel
		}
		values["OIDC_POST_LOGOUT_REDIRECT_URI"] = input.PublicURL
		values["OIDC_SESSION_COOKIE_SECURE"] = booleanEnvironmentValue(strings.HasPrefix(strings.ToLower(input.Issuer), "https://"))
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
		values["CONTRACT_MACHINE_TOKEN_ISSUER"] = "basic-platform"
		values["CONTRACT_MACHINE_TOKEN_AUDIENCE"] = "basic-platform-application"
		values["CONTRACT_MACHINE_TOKEN_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
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

	// The generated file is always .env.local, which is the documented subsystem Compose
	// convention. --env-file also makes the same values available during Compose interpolation.
	var startErr error
	switch input.ApplicationCode {
	case integratedContractApplicationCode:
		startErr = provisioner.rebuildIntegratedContractStack(operationCtx)
	case integratedCustomerApplicationCode:
		startErr = provisioner.rebuildIntegratedCustomerStack(operationCtx)
	case integratedPortalApplicationCode:
		startErr = provisioner.rebuildIntegratedPortalStack(operationCtx)
	case integratedProjectApplicationCode:
		startErr = provisioner.rebuildIntegratedProjectStack(operationCtx)
	default:
		startErr = provisioner.runner.Run(operationCtx, projectDirectory, os.Environ(), provisioner.config.DockerBinary,
			"compose", "--project-directory", projectDirectory, "--env-file", environmentPath, "-f", composeFile, "up", "-d", "--build")
	}
	if startErr != nil {
		return provisioningError("start subsystem containers")
	}

	// contract_management is compiled into the unified frontend and its API routes are already
	// part of nginx/default.conf. Reloading that same Nginx while it is proxying this onboarding
	// request can close the client-side upstream connection just before the successful 201 response.
	// Only independently deployed subsystems need a generated whole-site proxy entry.
	if !isIntegratedSubsystem(input.ApplicationCode) {
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

	// Post-onboarding authorization catalog sync. The hook only fires for the configured target
	// application code (contract_management today) and only when the operator has supplied the
	// required image + database coordinates. The script is best-effort: failures are logged but
	// do not block the onboarding response, so a missing publisher client in the seed data
	// cannot strand a new subsystem in an unrecoverable state.
	if err := provisioner.maybeSyncContractCatalogLocked(operationCtx, input); err != nil {
		if provisioner.config.ProjectsRoot == "" {
			// logger is intentionally not available in the runner; surface via stderr fallback.
			fmt.Fprintf(os.Stderr, "[subsystem-provisioner] contract catalog sync skipped or failed: %v\n", err)
		}
	}
	return nil
}

func requiredContractServiceCredentials(input application.SubsystemProvisioningInput) (map[string]application.SubsystemServiceCredential, error) {
	purposes := []string{
		application.ServiceCredentialAuditIngest,
		application.ServiceCredentialContractOpportunitySignedWrite,
		application.ServiceCredentialContractSummaryRead,
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

// rebuildIntegratedContractStack 让 contract_management 保持在工作区唯一的本地
// Compose topology. The unified frontend already routes to basic-platform-local/contract-api;
// starting the subsystem's standalone Compose file as well would create a second contract-api
// network alias backed by a different MySQL volume and make requests non-deterministic.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedContractStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "contract-mysql", "temporal"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "contract-migrate"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--build", "--no-deps", "contract-api")
}

// rebuildIntegratedProjectStack 让 project_management 保持在工作区唯一的本地
// Compose topology. The unified frontend already routes to basic-platform-local/project-api;
// starting the subsystem's standalone Compose file as well would create a second project-api
// network alias backed by a different MySQL volume and make requests non-deterministic.
func (provisioner *LocalDockerSubsystemProvisioner) rebuildIntegratedProjectStack(ctx context.Context) error {
	if err := provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--no-deps", "project-mysql"); err != nil {
		return err
	}
	if err := provisioner.runIntegratedPlatformCompose(ctx, "run", "--rm", "--no-deps", "project-migrate"); err != nil {
		return err
	}
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--build", "--no-deps", "project-api")
}

// rebuildIntegratedCustomerStack publishes the application-owned authorization catalog before
// starting customer-api in OIDC mode. Catalog publication is a one-shot deployment operation;
// normal API restarts therefore do not depend on a long-lived publisher credential remaining valid.
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

// rebuildIntegratedPortalStack uses an isolated Portal database and process while reusing the
// customer repository's immutable backend image. The catalog is published before the Portal is
// started, then customer-api is recreated so invitation endpoints receive the generated,
// least-privilege machine credentials.
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
	return provisioner.runIntegratedPlatformCompose(ctx, "up", "-d", "--wait", "--force-recreate", "--no-deps", "customer-api")
}

func (provisioner *LocalDockerSubsystemProvisioner) runIntegratedPlatformCompose(ctx context.Context, arguments ...string) error {
	platformRoot, workspaceRoot, composeFile, platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment, err := provisioner.integratedComposeConfiguration()
	if err != nil {
		return err
	}
	// Integrated subsystem rebuilds must be deterministic and must not inherit a
	// stale LAN override from the shell that started the platform API. The
	// regular docker-local entrypoint sets these values explicitly; the
	// provisioner runs Compose from inside the API container and therefore has
	// to do the same. Otherwise Compose can resolve an old, absent
	// platform/docker/.env.lan and leave contract-api in Created state.
	lanPlaceholder := filepath.Join(platformRoot, "docker", ".env.lan.disabled")
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
	lanOverrideKeys := map[string]struct{}{
		"BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE": {},
		"CONTRACT_LAN_OVERRIDE_ENV_FILE":       {},
		"CUSTOMER_LAN_OVERRIDE_ENV_FILE":       {},
		"PORTAL_LAN_OVERRIDE_ENV_FILE":         {},
		"PROJECT_LAN_OVERRIDE_ENV_FILE":        {},
	}
	runnerEnvironment := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, isLANOverride := lanOverrideKeys[key]; isLANOverride {
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
		"BASIC_PLATFORM_HOST_PROJECT_ROOT="+platformRoot,
		"SUBSYSTEM_HOST_PROJECTS_ROOT="+workspaceRoot,
	)
	return provisioner.runner.Run(ctx, platformRoot, runnerEnvironment, provisioner.config.DockerBinary, composeArguments...)
}

func (provisioner *LocalDockerSubsystemProvisioner) integratedComposeConfiguration() (string, string, string, string, string, string, string, string, error) {
	platformRoot := filepath.Dir(filepath.Dir(provisioner.config.GatewayScriptPath))
	workspaceRoot := filepath.Dir(platformRoot)
	composeFile := filepath.Join(platformRoot, "compose.local.yaml")
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
	// Existing local installations predate customer_portal. Other integrated subsystems must
	// remain operable without forcing an immediate new environment file. Portal preflight checks
	// its own template separately, and Provision writes .env.portal.local before Compose is run.
	if info, err := os.Stat(portalEnvironment); err != nil || info.IsDir() {
		portalEnvironment = os.DevNull
	}
	// Existing local installations predate project_management. The project environment file is
	// written by Provision before the integrated stack is started; treat a missing file the same
	// as Portal so pre-existing workspaces keep working until the first onboarding.
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
	case integratedContractApplicationCode, integratedCustomerApplicationCode, integratedPortalApplicationCode, integratedProjectApplicationCode:
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

// portalRuntimeSecrets preserves already generated local material and fills every secret on the
// first Agent-driven onboarding. The DSN is always rebuilt from the same generated database
// password, avoiding a template placeholder or a second unrelated random value in the connection
// string. Browser and machine OAuth credentials are intentionally handled by the caller instead.
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
	for _, key := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64"} {
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

// maybeSyncContractCatalogLocked runs the platform's catalog sync helper image for the configured
// target subsystem. The helper pulls the application-owned role/permission manifest out of the
// platform's MySQL, mints a catalog-publisher access token, and PUTs the manifest back to the
// platform's /authorization-catalog endpoint using the subsystem's own service credential.
//
// The helper is launched as a one-shot `docker run --rm --network=host` from the provisioner
// (which has no internal network of its own). Failure to sync is non-fatal: the operator can
// always re-run the script out of band, and the regular handbook flow remains usable.
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
	if strings.TrimSpace(provisioner.config.CatalogSyncImage) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlContainer) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlUser) == "" ||
		strings.TrimSpace(provisioner.config.CatalogSyncMysqlPassword) == "" {
		return fmt.Errorf("catalog sync image / MySQL coordinates are not fully configured")
	}
	arguments := []string{
		"run", "--rm", "--network=host",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		// `-e NAME` forwards the runner environment without placing secret values in
		// docker's argv (and therefore /proc/<pid>/cmdline / command audit logs).
		"-e", "PLATFORM_APPLICATION_ID",
		"-e", "PLATFORM_BASE_URL",
		"-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID",
		"-e", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET",
		"-e", "PLATFORM_MYSQL_CONTAINER",
		"-e", "PLATFORM_MYSQL_USER",
		"-e", "PLATFORM_MYSQL_PASSWORD",
		"-e", "PLATFORM_MYSQL_DATABASE",
		provisioner.config.CatalogSyncImage,
		"/usr/local/bin/sync-contract-catalog.sh",
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
	return provisioner.runner.Run(operationCtx, "/var/run/docker.sock", runnerEnvironment, provisioner.config.DockerBinary, arguments...)
}

// logTeardownLeftovers removes the gateway entry and .env.local for a subsystem whose Compose
// project is already gone. Called from Teardown when the compose file lookup fails; the user
// intent is still "tear this subsystem down", so we must scrub everything that is left.
func logTeardownLeftovers(runner subsystemCommandRunner, ctx context.Context, projectDirectory, applicationCode string) {
	environmentPath := filepath.Join(projectDirectory, ".env.local")
	_ = os.Remove(environmentPath)
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

// ApplyAccess rewrites the local override environment files and recreates the affected
// containers so the unified frontend, OIDC issuer and subsystem callbacks use the configured
// public origin. An empty PublicOrigin restores local-only access (127.0.0.1). This mirrors
// scripts/lan-access.sh but is driven by the platform management console through the socket.
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

	origin := strings.TrimSpace(input.PublicOrigin)
	port := accessHTTPPort(origin)
	if origin == "" {
		_ = os.Remove(overrideFile)
		_ = os.Remove(customerOverrideFile)
		return provisioner.runAccessCompose(ctx, platformRoot, workspaceRoot, composeFile,
			platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment,
			placeholderFile, placeholderFile, "127.0.0.1", port,
			"up", "-d", "--no-deps", "--wait", "api", "contract-api", "customer-api", "frontend")
	}

	if err := writeAccessOverrideFiles(overrideFile, customerOverrideFile, origin, input.AllowInsecureHTTPRedirect); err != nil {
		return err
	}
	return provisioner.runAccessCompose(ctx, platformRoot, workspaceRoot, composeFile,
		platformEnvironment, contractEnvironment, customerEnvironment, portalEnvironment, projectEnvironment,
		overrideFile, customerOverrideFile, "0.0.0.0", port,
		"up", "-d", "--no-deps", "--wait", "api", "contract-api", "customer-api", "frontend")
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
	payload := "# 由平台「对外访问」配置自动生成；执行恢复本机访问会删除本文件。\n# 仅用于临时访问，不要提交到版本库。\n" + content
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return provisioningError("write " + description + ": " + err.Error())
	}
	return os.Chmod(path, 0o600)
}

func accessHTTPPort(origin string) string {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Port() == "" {
		return "8081"
	}
	return parsed.Port()
}
