package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	executor, err := infrastructure.NewLocalDockerSubsystemProvisioner(infrastructure.LocalDockerSubsystemProvisionerConfig{
		Enabled:                 true,
		ProjectsRoot:            os.Getenv("SUBSYSTEM_PROJECTS_ROOT"),
		GatewayScriptPath:       os.Getenv("SUBSYSTEM_GATEWAY_SCRIPT_PATH"),
		GatewayIncludePath:      os.Getenv("SUBSYSTEM_GATEWAY_INCLUDE_PATH"),
		PlatformComposeProject:  envOrDefault("SUBSYSTEM_PLATFORM_COMPOSE_PROJECT", "basic-platform-local"),
		PlatformFrontendService: envOrDefault("SUBSYSTEM_PLATFORM_FRONTEND_SERVICE", "frontend"),
		PlatformDockerNetwork:   envOrDefault("SUBSYSTEM_PLATFORM_DOCKER_NETWORK", "basic-platform-local_default"),
		DockerBinary:            envOrDefault("SUBSYSTEM_DOCKER_BINARY", "docker"),
		Timeout:                 timeout,
	})
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

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
