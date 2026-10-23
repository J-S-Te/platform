// Command subsystem-provisioner 运行具备 Docker/宿主机写权限的部署 Agent，并通过受限
// Unix Socket 接收 API 的子系统生命周期请求；它不是 AI 组件，也不承载业务 HTTP 流量。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	timeout := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("SUBSYSTEM_ONBOARDING_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			logger.Error("invalid subsystem provisioning timeout")
			os.Exit(1)
		}
		timeout = parsed
	}

	executor, err := subsystemExecutor(timeout)
	if err != nil {
		logger.Error("initialize subsystem provisioning executor", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	socketPath := envOrDefault("SUBSYSTEM_PROVISIONING_SOCKET_PATH", "/run/basic-platform-provisioner/provisioner.sock")
	logger.Info("subsystem provisioning helper started")
	if err := infrastructure.RunSubsystemProvisioningServer(ctx, socketPath, executor); err != nil {
		logger.Error("subsystem provisioning helper stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}

func subsystemExecutor(timeout time.Duration) (application.SubsystemProvisioner, error) {
	// 模式在进程启动时固定：local 可操作相邻源码与开发网关，production 只能操作批准的部署目录。
	// 不允许运行中由请求载荷切换执行器，避免 API 调用扩大宿主机访问范围。
	switch strings.ToLower(envOrDefault("SUBSYSTEM_ONBOARDING_MODE", "local")) {
	case "local":
		return infrastructure.NewLocalDockerSubsystemProvisioner(infrastructure.LocalDockerSubsystemProvisionerConfig{
			Enabled:                   true,
			ProjectsRoot:              os.Getenv("SUBSYSTEM_PROJECTS_ROOT"),
			GatewayScriptPath:         os.Getenv("SUBSYSTEM_GATEWAY_SCRIPT_PATH"),
			GatewayIncludePath:        os.Getenv("SUBSYSTEM_GATEWAY_INCLUDE_PATH"),
			PlatformComposeProject:    envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-local"),
			PlatformComposeFile:       envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_FILE", "compose.local.yaml"),
			PlatformFrontendService:   envOrDefault("SUBSYSTEM_PLATFORM_FRONTEND_SERVICE", "frontend"),
			PlatformDockerNetwork:     envOrDefault("SUBSYSTEM_PLATFORM_DOCKER_NETWORK", "basic-platform-local_default"),
			DockerBinary:              envOrDefault("SUBSYSTEM_DOCKER_BINARY", "docker"),
			Timeout:                   timeout,
			CatalogSyncEnabled:        envOrDefault("SUBSYSTEM_CATALOG_SYNC_ENABLED", "true") == "true",
			CatalogSyncImage:          envOrDefault("SUBSYSTEM_CATALOG_SYNC_IMAGE", "basic-platform/backend:local"),
			CatalogSyncMysqlContainer: envOrDefault("SUBSYSTEM_CATALOG_SYNC_MYSQL_CONTAINER", "basic-platform-local-mysql-1"),
			CatalogSyncMysqlUser:      envOrDefault("SUBSYSTEM_CATALOG_SYNC_MYSQL_USER", "basic_platform"),
			CatalogSyncMysqlPassword:  os.Getenv("SUBSYSTEM_CATALOG_SYNC_MYSQL_PASSWORD"),
			CatalogSyncMysqlDatabase:  envOrDefault("SUBSYSTEM_CATALOG_SYNC_MYSQL_DATABASE", "basic_platform"),
			CatalogSyncTargetAppCode:  envOrDefault("SUBSYSTEM_CATALOG_SYNC_TARGET_APP_CODE", "contract_management"),
			KeycloakPublicURL:         os.Getenv("SUBSYSTEM_KEYCLOAK_PUBLIC_URL"),
			KeycloakInternalURL:       os.Getenv("SUBSYSTEM_KEYCLOAK_INTERNAL_URL"),
			KeycloakRealm:             envOrDefault("SUBSYSTEM_KEYCLOAK_REALM", "basic-platform"),
		})
	case "production":
		releaseIntegrityCheck, err := releaseIntegrityCheckFromEnv()
		if err != nil {
			return nil, err
		}
		runtimeProjection, err := runtimeProjectionCheckFromEnv()
		if err != nil {
			return nil, err
		}
		return infrastructure.NewProductionComposeSubsystemProvisioner(infrastructure.ProductionComposeSubsystemProvisionerConfig{
			Enabled:           true,
			DeployRoot:        os.Getenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT"),
			ProfilesDirectory: os.Getenv("SUBSYSTEM_PRODUCTION_PROFILES_DIR"),
			RuntimeEnvPath:    os.Getenv("SUBSYSTEM_PRODUCTION_RUNTIME_ENV_PATH"),
			ReleaseEnvPath:    os.Getenv("SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH"),
			ComposeFile:       os.Getenv("SUBSYSTEM_PRODUCTION_COMPOSE_FILE"),
			AllowedTenantID:   os.Getenv("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID"),
			ComposeProject:    envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production"),
			DockerBinary:      envOrDefault("SUBSYSTEM_DOCKER_BINARY", "docker"),
			Timeout:           timeout,
			// 测试服务器可显式允许数据库占位凭据，让 Agent 先完成接入并由
			// Compose 在首次创建空数据库时使用该值；生产默认关闭此开关。
			AllowPlaceholderDatabaseCredentials: strings.EqualFold(
				strings.TrimSpace(os.Getenv("SUBSYSTEM_PRODUCTION_ALLOW_PLACEHOLDER_DATABASE_CREDENTIALS")),
				"true",
			),
			ReleaseIntegrityCheck:  releaseIntegrityCheck,
			RuntimeProjectionCheck: runtimeProjection,
		})
	default:
		return nil, fmt.Errorf("unsupported SUBSYSTEM_ONBOARDING_MODE")
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// releaseIntegrityCheckFromEnv 返回可选的、受锁保护的写入前
// 检查。仅在 SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED=true 时启用。
// 启用后，验证器和信任目录必须已通过
// 独立可信通道安装；Agent 绝不从
// 候选部署或交付介质读取信任材料。
func releaseIntegrityCheckFromEnv() (func(context.Context, string, string) error, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED")), "true") {
		return nil, nil
	}
	verifier := filepath.Clean(envOrDefault("SUBSYSTEM_RELEASE_VERIFIER", "/usr/local/bin/release-verifier"))
	trustDir := filepath.Clean(envOrDefault("SUBSYSTEM_RELEASE_TRUST_DIR", "/etc/unified-identity-platform/release-trust"))
	expectedDigest := strings.TrimSpace(os.Getenv("SUBSYSTEM_RELEASE_EXPECTED_DIGEST"))
	if expectedDigest != "" && !validReleaseDigest(expectedDigest) {
		return nil, fmt.Errorf("SUBSYSTEM_RELEASE_EXPECTED_DIGEST must be sha256:<64 lowercase hex>")
	}
	if !filepath.IsAbs(verifier) || !filepath.IsAbs(trustDir) {
		return nil, fmt.Errorf("release verifier and trust directory must be absolute paths")
	}
	info, err := os.Lstat(verifier)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("trusted release verifier is unavailable")
	}
	trustInfo, err := os.Lstat(trustDir)
	if err != nil || !trustInfo.IsDir() || trustInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("trusted release trust directory is unavailable")
	}
	return func(ctx context.Context, root, requestedDigest string) error {
		boundDigest := strings.TrimSpace(requestedDigest)
		if boundDigest == "" {
			boundDigest = expectedDigest
		}
		if boundDigest != "" && !validReleaseDigest(boundDigest) {
			return fmt.Errorf("requested release digest is malformed")
		}
		commandArgs := []string{"check-installed", "--target", root, "--trust-dir", trustDir, "--output", "json"}
		if boundDigest != "" {
			commandArgs = append(commandArgs, "--expected-release-digest", boundDigest)
		}
		command := exec.CommandContext(ctx, verifier, commandArgs...)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("release integrity verifier failed")
		}
		var result struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(output, &result); err != nil || result.Status != "INSTALLED_MANIFEST_VERIFIED" {
			return fmt.Errorf("release integrity verifier did not return INSTALLED_MANIFEST_VERIFIED")
		}
		return nil
	}, nil
}

func validReleaseDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

var composeProjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// runtimeProjectionCheckFromEnv 验证所配置 Compose 项目中
// 所有运行容器使用的镜像配置摘要均包含在已签名的
// 发布清单内。此检查不验证服务副本或 Compose
// 插值；这些内容仍须单独验收。
func runtimeProjectionCheckFromEnv() (func(context.Context) error, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED")), "true") {
		return nil, nil
	}
	root := filepath.Clean(strings.TrimSpace(os.Getenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT")))
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT must be absolute for runtime projection")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("release deployment root is unavailable for runtime projection")
	}
	project := strings.TrimSpace(envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production"))
	if !composeProjectPattern.MatchString(project) {
		return nil, fmt.Errorf("Compose project name is invalid for runtime projection")
	}
	docker := strings.TrimSpace(envOrDefault("SUBSYSTEM_DOCKER_BINARY", "docker"))
	if docker == "" || strings.ContainsAny(docker, "/\\\r\n\x00") {
		return nil, fmt.Errorf("Docker binary must be a command name for runtime projection")
	}
	manifestPath := filepath.Join(root, "runtime", "release-integrity", "release-manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("signed release manifest is unavailable for runtime projection")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("signed release manifest is unreadable for runtime projection")
	}
	var manifest struct {
		Images []struct {
			ConfigDigest string `json:"config_digest"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("signed release manifest is invalid for runtime projection")
	}
	allowed := make(map[string]bool, len(manifest.Images))
	for _, image := range manifest.Images {
		digest := strings.TrimSpace(image.ConfigDigest)
		if digest == "" {
			return nil, fmt.Errorf("signed release manifest has an image without config digest")
		}
		allowed[digest] = true
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("signed release manifest has no images for runtime projection")
	}
	return func(ctx context.Context) error {
		output, err := exec.CommandContext(ctx, docker, "ps", "-q", "--filter", "label=com.docker.compose.project="+project).Output()
		if err != nil {
			return fmt.Errorf("cannot list running Compose containers")
		}
		ids := strings.Fields(string(output))
		if len(ids) == 0 {
			return nil
		}
		args := append([]string{"inspect", "--format", "{{.Image}}"}, ids...)
		output, err = exec.CommandContext(ctx, docker, args...).Output()
		if err != nil {
			return fmt.Errorf("cannot inspect running Compose images")
		}
		images := strings.Fields(string(output))
		if len(images) != len(ids) {
			return fmt.Errorf("running Compose image inventory is incomplete")
		}
		for _, image := range images {
			if !allowed[strings.TrimSpace(image)] {
				return fmt.Errorf("running Compose image is outside the signed release")
			}
		}
		return nil
	}, nil
}
