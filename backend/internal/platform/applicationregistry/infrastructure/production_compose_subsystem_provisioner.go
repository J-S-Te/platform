package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

// ProductionComposeSubsystemProvisionerConfig 固定运维方掌控的生产部署目录。所有路径、Compose
// 项目和可执行文件都来自服务端配置，浏览器输入不能选择主机文件、镜像、服务或命令。
type ProductionComposeSubsystemProvisionerConfig struct {
	Enabled         bool
	DeployRoot      string
	RuntimeEnvPath  string
	ContractEnvPath string
	ReleaseEnvPath  string
	ComposeFile     string
	AllowedTenantID string
	ApplicationCode string
	Environment     string
	PathPrefix      string
	UpstreamURL     string
	DependencyServices []string
	DatabaseService    string
	DatabaseName       string
	MigrateService     string
	APIService         string
	ReleaseImageKey    string
	ComposeProject  string
	DockerBinary    string
	Timeout         time.Duration
}

// ProductionComposeSubsystemProvisioner delivers one-time integration credentials to the
// already-installed production stack and recreates only the contract runtime. It deliberately
// supports no arbitrary project discovery or browser-selected Compose operation.
type ProductionComposeSubsystemProvisioner struct {
	config ProductionComposeSubsystemProvisionerConfig
	runner subsystemCommandRunner
	mutex  sync.Mutex
}

// NewProductionComposeSubsystemProvisioner constructs the production deployment adapter.
func NewProductionComposeSubsystemProvisioner(config ProductionComposeSubsystemProvisionerConfig) (*ProductionComposeSubsystemProvisioner, error) {
	return newProductionComposeSubsystemProvisioner(config, execSubsystemCommandRunner{})
}

func newProductionComposeSubsystemProvisioner(config ProductionComposeSubsystemProvisionerConfig, runner subsystemCommandRunner) (*ProductionComposeSubsystemProvisioner, error) {
	if runner == nil {
		return nil, errors.New("production subsystem command runner is required")
	}
	config.DeployRoot = strings.TrimSpace(config.DeployRoot)
	if config.DeployRoot == "" {
		return nil, errors.New("production subsystem deploy root is required")
	}
	absoluteRoot, err := filepath.Abs(config.DeployRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve production deploy root: %w", err)
	}
	config.DeployRoot = filepath.Clean(absoluteRoot)
	config.AllowedTenantID = strings.TrimSpace(config.AllowedTenantID)
	if config.AllowedTenantID == "" {
		return nil, errors.New("production subsystem allowed tenant is required")
	}
	config.ApplicationCode = strings.ToLower(strings.TrimSpace(config.ApplicationCode))
	config.Environment = strings.ToLower(strings.TrimSpace(config.Environment))
	config.PathPrefix = strings.TrimRight(strings.TrimSpace(config.PathPrefix), "/")
	config.UpstreamURL = strings.TrimRight(strings.TrimSpace(config.UpstreamURL), "/")
	if !validProductionTargetCode(config.ApplicationCode, 64) || !validProductionTargetCode(config.Environment, 16) ||
		!validProductionPathPrefix(config.PathPrefix) || !validProductionUpstreamURL(config.UpstreamURL) {
		return nil, errors.New("production subsystem target configuration is invalid")
	}
	config.DependencyServices = normalizedProductionServices(config.DependencyServices)
	config.DatabaseService = strings.TrimSpace(config.DatabaseService)
	config.DatabaseName = strings.TrimSpace(config.DatabaseName)
	config.MigrateService = strings.TrimSpace(config.MigrateService)
	config.APIService = strings.TrimSpace(config.APIService)
	config.ReleaseImageKey = strings.TrimSpace(config.ReleaseImageKey)
	if len(config.DependencyServices) == 0 || !validProductionComposeService(config.DatabaseService) || !validProductionDatabaseName(config.DatabaseName) ||
		!validProductionComposeService(config.MigrateService) || !validProductionComposeService(config.APIService) || !validProductionReleaseImageKey(config.ReleaseImageKey) {
		return nil, errors.New("production subsystem Compose target configuration is invalid")
	}
	if config.RuntimeEnvPath = strings.TrimSpace(config.RuntimeEnvPath); config.RuntimeEnvPath == "" {
		config.RuntimeEnvPath = filepath.Join(config.DeployRoot, ".env")
	}
	if config.ContractEnvPath = strings.TrimSpace(config.ContractEnvPath); config.ContractEnvPath == "" {
		config.ContractEnvPath = filepath.Join(config.DeployRoot, "runtime", "contract.env")
	}
	if config.ReleaseEnvPath = strings.TrimSpace(config.ReleaseEnvPath); config.ReleaseEnvPath == "" {
		config.ReleaseEnvPath = filepath.Join(config.DeployRoot, ".release.env")
	}
	if config.ComposeFile = strings.TrimSpace(config.ComposeFile); config.ComposeFile == "" {
		config.ComposeFile = filepath.Join(config.DeployRoot, "compose.yaml")
	}
	if config.ComposeProject = strings.TrimSpace(config.ComposeProject); config.ComposeProject == "" {
		config.ComposeProject = "basic-platform-production"
	}
	if config.DockerBinary = strings.TrimSpace(config.DockerBinary); config.DockerBinary == "" {
		config.DockerBinary = "docker"
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Minute
	}
	return &ProductionComposeSubsystemProvisioner{config: config, runner: runner}, nil
}

// Preflight validates the immutable production deployment boundary before the platform creates
// OAuth credentials. Only the built-in contract application is currently supported.
func (provisioner *ProductionComposeSubsystemProvisioner) Preflight(ctx context.Context, input application.SubsystemPreflightInput) error {
	if !provisioner.config.Enabled {
		return provisioningError("production subsystem deployment is disabled")
	}
	if err := provisioner.validatePreflightInput(input); err != nil {
		return err
	}
	if strings.TrimSpace(input.TenantID) != provisioner.config.AllowedTenantID {
		return provisioningError("production subsystem tenant is not allowed")
	}
	if strings.ToLower(strings.TrimSpace(input.ApplicationCode)) != provisioner.config.ApplicationCode {
		return provisioningError("production subsystem target is not allowed")
	}
	if err := provisioner.validateDeploymentFiles(true); err != nil {
		return err
	}
	checkContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := provisioner.runner.Run(checkContext, provisioner.config.DeployRoot, os.Environ(), provisioner.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return provisioningError("Docker service is unavailable")
	}
	if err := provisioner.runCompose(checkContext, "config", "--quiet"); err != nil {
		return provisioningError("production Compose configuration is invalid")
	}
	return nil
}

// Provision 串行写入平台管理的 OIDC 配置、执行迁移并等待合同 API 健康。进程互斥锁解决
// 单 Agent 并发，文件锁则与主机上的 CI/CD 脚本共享排他边界，避免两条发布链互相覆盖环境文件。
func (provisioner *ProductionComposeSubsystemProvisioner) Provision(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	if err := provisioner.validateProvisioningInput(input); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(provisioner.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)

	secureCookie := strings.EqualFold(mustParseURL(input.Issuer).Scheme, "https")
	auditCredential, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest)
	if !ok {
		return provisioningError("production contract audit credential is incomplete")
	}
	values := map[string]string{
		"OIDC_ISSUER":                                   strings.TrimRight(input.Issuer, "/"),
		"OIDC_CLIENT_ID":                                input.ClientID,
		"OIDC_CLIENT_SECRET":                            input.ClientSecret,
		"OIDC_REDIRECT_URI":                             input.RedirectURI,
		"OIDC_POST_LOGOUT_REDIRECT_URI":                 strings.TrimRight(input.PublicURL, "/") + "/logged-out",
		"OIDC_SCOPES":                                   "openid profile",
		"OIDC_TENANT_ID":                                input.TenantID,
		"OIDC_SESSION_COOKIE_NAME":                      provisioner.config.ApplicationCode + "_session",
		"OIDC_SESSION_COOKIE_SECURE":                    booleanEnvironmentValue(secureCookie),
		"APP_PUBLIC_URL":                                input.PublicURL,
		"APP_PATH_PREFIX":                               input.PathPrefix,
		"PLATFORM_APPLICATION_CODE":                     provisioner.config.ApplicationCode,
		"PLATFORM_ENVIRONMENT_CODE":                     provisioner.config.Environment,
		"PLATFORM_AUDIT_CLIENT_ID":                      auditCredential.OAuthClient.ClientID,
		"PLATFORM_AUDIT_CLIENT_SECRET":                  auditCredential.PlaintextSecret,
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "true",
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": input.ApplicationID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      input.CatalogPublisherClientID,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  input.CatalogPublisherClientSecret,
	}
	if err := updateProductionSubsystemEnvironment(provisioner.config.ContractEnvPath, values); err != nil {
		return provisioningError("write production contract runtime configuration")
	}
	if err := provisioner.deployContractLocked(operationContext); err != nil {
		return err
	}
	return nil
}

// Update reapplies the already-delivered production configuration. OAuth secrets are not
// recoverable from the platform database and therefore are never rotated implicitly here.
func (provisioner *ProductionComposeSubsystemProvisioner) Update(ctx context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	if !provisioner.matchesTarget(input.ApplicationCode, input.Environment) {
		return provisioningError("production subsystem deployment request is invalid")
	}
	if err := provisioner.validateTenant(input.TenantID); err != nil {
		return err
	}
	if err := provisioner.validateDeploymentFiles(false); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(provisioner.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)
	return provisioner.deployTargetLocked(operationContext)
}

// Teardown stops only the contract API. Databases, runtime secrets, release pointers, platform
// containers, and backups remain untouched so an explicit recovery remains possible.
func (provisioner *ProductionComposeSubsystemProvisioner) Teardown(ctx context.Context, tenantID, applicationCode, environment string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	if !provisioner.matchesTarget(applicationCode, environment) {
		return provisioningError("production subsystem teardown request is invalid")
	}
	if err := provisioner.validateTenant(tenantID); err != nil {
		return err
	}
	if err := provisioner.validateDeploymentFiles(false); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, provisioner.config.Timeout)
	defer cancel()
	lock, err := acquireProvisioningFileLock(operationContext, filepath.Join(provisioner.config.DeployRoot, "runtime", ".deploy.lock"))
	if err != nil {
		return provisioningError("production deployment lock is unavailable")
	}
	defer releaseProvisioningFileLock(lock)
	if err := provisioner.runCompose(operationContext, "stop", provisioner.config.APIService); err != nil {
		return provisioningError("stop production subsystem API")
	}
	return nil
}

func (provisioner *ProductionComposeSubsystemProvisioner) deployTargetLocked(ctx context.Context) error {
	dependencyArguments := []string{"up", "-d", "--wait", "--wait-timeout", "240"}
	dependencyArguments = append(dependencyArguments, provisioner.config.DependencyServices...)
	if err := provisioner.runCompose(ctx, dependencyArguments...); err != nil {
		return provisioningError("start production subsystem dependencies")
	}
	backupName := provisioner.config.ApplicationCode + "-onboarding-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".sql"
	if err := os.MkdirAll(filepath.Join(provisioner.config.DeployRoot, "backups"), 0o750); err != nil {
		return provisioningError("prepare production contract backup")
	}
	if err := provisioner.runCompose(ctx, "exec", "-T", provisioner.config.DatabaseService, "sh", "-c",
		`set -eu; umask 077; exec mysqldump --single-transaction --routines --triggers -uroot -p"$MYSQL_ROOT_PASSWORD" "$2" > "/backups/$1"`,
		"_", backupName, provisioner.config.DatabaseName); err != nil {
		return provisioningError("backup production subsystem database")
	}
	if err := provisioner.runCompose(ctx, "--profile", "release", "run", "--rm", "--no-deps", provisioner.config.MigrateService); err != nil {
		return provisioningError("migrate production subsystem database")
	}
	if err := provisioner.runCompose(ctx, "up", "-d", "--wait", "--wait-timeout", "240", "--force-recreate", "--no-deps", provisioner.config.APIService); err != nil {
		return provisioningError("start production subsystem API")
	}
	return nil
}

func (provisioner *ProductionComposeSubsystemProvisioner) runCompose(ctx context.Context, arguments ...string) error {
	prefix := []string{
		"compose", "--project-name", provisioner.config.ComposeProject,
		"--project-directory", provisioner.config.DeployRoot,
		"--env-file", provisioner.config.RuntimeEnvPath,
		"--env-file", provisioner.config.ReleaseEnvPath,
		"--file", provisioner.config.ComposeFile,
	}
	runnerEnvironment := append([]string{}, os.Environ()...)
	runnerEnvironment = append(runnerEnvironment, "BASIC_PLATFORM_RUNTIME_ENV_FILE="+provisioner.config.RuntimeEnvPath)
	runnerEnvironment = append(runnerEnvironment, "CONTRACT_RUNTIME_ENV_FILE="+provisioner.config.ContractEnvPath)
	return provisioner.runner.Run(ctx, provisioner.config.DeployRoot, runnerEnvironment, provisioner.config.DockerBinary, append(prefix, arguments...)...)
}

func (provisioner *ProductionComposeSubsystemProvisioner) validateDeploymentFiles(requireWritableEnvironment bool) error {
	// EvalSymlinks 后必须仍位于固定部署根目录，防止运维目录中的符号链接把 Agent 的受控写入
	// 转向主机其他文件；运行时环境文件还要求组和其他用户均无权限。
	root, err := filepath.EvalSymlinks(provisioner.config.DeployRoot)
	if err != nil {
		return provisioningError("production deployment directory is unavailable")
	}
	if root != provisioner.config.DeployRoot {
		return provisioningError("production deployment directory must use its canonical path")
	}
	for _, path := range []string{provisioner.config.RuntimeEnvPath, provisioner.config.ContractEnvPath, provisioner.config.ReleaseEnvPath, provisioner.config.ComposeFile} {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil || !pathWithinRoot(root, resolved) {
			return provisioningError("production deployment file is unavailable")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.Mode().IsRegular() {
			return provisioningError("production deployment file is unavailable")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return provisioningError("production deployment files must not be group or world writable")
		}
	}
	if info, statErr := os.Stat(provisioner.config.RuntimeEnvPath); statErr != nil || info.Mode().Perm()&0o077 != 0 {
		return provisioningError("production runtime environment permissions must be 0600")
	}
	if info, statErr := os.Stat(provisioner.config.ContractEnvPath); statErr != nil || info.Mode().Perm()&0o077 != 0 {
		return provisioningError("production contract environment permissions must be 0600")
	}
	if err := validateProductionRuntimePrerequisites(provisioner.config.RuntimeEnvPath); err != nil {
		return err
	}
	if err := validateProductionReleaseImage(provisioner.config.ReleaseEnvPath, provisioner.config.ReleaseImageKey); err != nil {
		return err
	}
	if requireWritableEnvironment {
		temporary, createErr := os.CreateTemp(filepath.Dir(provisioner.config.ContractEnvPath), ".provisioning-write-check.*")
		if createErr != nil {
			return provisioningError("production runtime environment is not writable")
		}
		temporaryPath := temporary.Name()
		closeErr := temporary.Close()
		removeErr := os.Remove(temporaryPath)
		if closeErr != nil || removeErr != nil {
			return provisioningError("production runtime environment is not writable")
		}
	}
	return nil
}

func validateProductionRuntimePrerequisites(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return provisioningError("production runtime environment is unavailable")
	}
	values := parseEnvironmentValues(string(content))
	for _, key := range []string{
		"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD", "IAM_MOBILE_ENCRYPTION_KEY", "IAM_BOOTSTRAP_TOKEN",
		"CONTRACT_MYSQL_PASSWORD", "CONTRACT_MYSQL_ROOT_PASSWORD",
	} {
		value := strings.TrimSpace(values[key])
		if value == "" || strings.HasPrefix(value, "REPLACE_WITH_") || strings.HasPrefix(value, "PENDING_") {
			return provisioningError("production infrastructure secrets are incomplete")
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
		return provisioningError("production contract image must use an immutable digest")
	}
	for _, character := range value[index+len(marker):] {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return provisioningError("production contract image must use an immutable digest")
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

func (provisioner *ProductionComposeSubsystemProvisioner) validateProvisioningInput(input application.SubsystemProvisioningInput) error {
	if !provisioner.matchesTarget(input.ApplicationCode, input.Environment) {
		return provisioningError("production subsystem deployment request is invalid")
	}
	if err := provisioner.validateTenant(input.TenantID); err != nil {
		return err
	}
	issuer := strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	expectedPublicURL := issuer + provisioner.config.PathPrefix + "/"
	expectedRedirectURI := issuer + provisioner.config.PathPrefix + "/auth/callback"
	if mustParseURL(issuer) == nil || strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.ApplicationID) == "" ||
		strings.TrimSpace(input.ClientID) == "" || strings.TrimSpace(input.ClientSecret) == "" ||
		strings.TrimSpace(input.CatalogPublisherClientID) == "" || strings.TrimSpace(input.CatalogPublisherClientSecret) == "" ||
		input.PathPrefix != provisioner.config.PathPrefix || input.UpstreamURL != provisioner.config.UpstreamURL ||
		input.PublicURL != expectedPublicURL || input.RedirectURI != expectedRedirectURI {
		return provisioningError("production contract integration values are inconsistent")
	}
	if _, ok := input.ServiceCredential(application.ServiceCredentialAuditIngest); !ok {
		return provisioningError("production contract audit credential is incomplete")
	}
	return provisioner.validateDeploymentFiles(true)
}

func (provisioner *ProductionComposeSubsystemProvisioner) validateTenant(tenantID string) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || tenantID != provisioner.config.AllowedTenantID || strings.ContainsAny(tenantID, "\r\n\x00") {
		return provisioningError("production subsystem tenant is not allowed")
	}
	return nil
}

func (provisioner *ProductionComposeSubsystemProvisioner) validatePreflightInput(input application.SubsystemPreflightInput) error {
	issuer := strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	expectedPublicBaseURL := issuer
	if mustParseURL(issuer) == nil || strings.ToLower(strings.TrimSpace(input.ApplicationCode)) != provisioner.config.ApplicationCode ||
		strings.ToLower(strings.TrimSpace(input.Environment)) != provisioner.config.Environment ||
		strings.ToLower(strings.TrimSpace(input.ClientType)) != "confidential" ||
		strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/") != expectedPublicBaseURL ||
		strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/") != provisioner.config.UpstreamURL ||
		strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/") != provisioner.config.PathPrefix {
		return provisioningError("production subsystem preflight values are inconsistent")
	}
	return nil
}

func (provisioner *ProductionComposeSubsystemProvisioner) matchesTarget(applicationCode, environment string) bool {
	return provisioner.config.Enabled &&
		strings.ToLower(strings.TrimSpace(applicationCode)) == provisioner.config.ApplicationCode &&
		strings.ToLower(strings.TrimSpace(environment)) == provisioner.config.Environment
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
	return strings.HasPrefix(value, "/") && value != "/" && !strings.Contains(value, "//") && !strings.Contains(value, "..") && !strings.ContainsAny(value, "?#\\\r\n\x00")
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
	// 非阻塞 flock 配合短轮询，使等待可响应请求取消/超时；锁文件描述符保持打开即代表持锁，
	// 因而可与 shell 中使用同一路径的 flock 跨进程互斥。
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
