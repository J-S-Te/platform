// Command subsystem-provisioner 运行具备 Docker/宿主机写权限的部署 Agent，并通过受限
// Unix Socket 接收 API 的子系统生命周期请求；它不是 AI 组件，也不承载业务 HTTP 流量。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
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
		logger.Error("initialize subsystem provisioning executor")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	socketPath := envOrDefault("SUBSYSTEM_PROVISIONING_SOCKET_PATH", "/run/basic-platform-provisioner/provisioner.sock")
	logger.Info("subsystem provisioning helper started")
	if err := infrastructure.RunSubsystemProvisioningServer(ctx, socketPath, executor); err != nil {
		logger.Error("subsystem provisioning helper stopped unexpectedly")
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
		})
	case "production":
		return infrastructure.NewProductionComposeSubsystemProvisioner(infrastructure.ProductionComposeSubsystemProvisionerConfig{
			Enabled:        true,
			DeployRoot:     os.Getenv("SUBSYSTEM_PRODUCTION_DEPLOY_ROOT"),
			RuntimeEnvPath: os.Getenv("SUBSYSTEM_PRODUCTION_RUNTIME_ENV_PATH"),
			ContractEnvPath: os.Getenv("SUBSYSTEM_PRODUCTION_CONTRACT_ENV_PATH"),
			ReleaseEnvPath: os.Getenv("SUBSYSTEM_PRODUCTION_RELEASE_ENV_PATH"),
			ComposeFile:    os.Getenv("SUBSYSTEM_PRODUCTION_COMPOSE_FILE"),
			AllowedTenantID: os.Getenv("SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID"),
			ComposeProject: envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-production"),
			DockerBinary:   envOrDefault("SUBSYSTEM_DOCKER_BINARY", "docker"),
			Timeout:        timeout,
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
