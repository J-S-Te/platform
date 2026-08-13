package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const (
	dockerDiscoveryLabel   = "com.basic-platform.discovery"
	dockerApplicationLabel = "com.basic-platform.application_code"
	dockerEnvironmentLabel = "com.basic-platform.environment"
)

// discoverDockerLabelCandidates inventories self-declared services before platform
// registration.  Unlike discoverDockerLabelServices it must not require platform-generated
// tenant/application/environment IDs, otherwise first-time discovery would be impossible.
func discoverDockerLabelCandidates(ctx context.Context, dockerBinary string) ([]application.SubsystemDiscoveryCandidate, error) {
	dockerBinary = strings.TrimSpace(dockerBinary)
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	ids, err := runDockerDiscoveryCommand(ctx, dockerBinary, "ps", "--filter", "label="+dockerDiscoveryLabel+"=v1", "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("discover Docker subsystem candidates: %w", err)
	}
	candidates := make([]application.SubsystemDiscoveryCandidate, 0)
	for _, id := range strings.Fields(ids) {
		labelsJSON, inspectErr := runDockerDiscoveryCommand(ctx, dockerBinary, "inspect", "--format", "{{json .Config.Labels}}", id)
		if inspectErr != nil {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(strings.TrimSpace(labelsJSON)), &labels) != nil {
			continue
		}
		health, _ := runDockerDiscoveryCommand(ctx, dockerBinary, "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", id)
		if candidate, ok := subsystemDiscoveryCandidateFromDockerLabels(labels, strings.TrimSpace(health)); ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// discoverDockerLabelServices uses labels already attached to containers by Compose.
// It deliberately uses only the Docker CLI that the existing Agent already depends on;
// there is no registry, daemon configuration, or host-file mutation involved.
func discoverDockerLabelServices(ctx context.Context, dockerBinary, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	applicationCode = strings.ToLower(strings.TrimSpace(applicationCode))
	environment = strings.ToLower(strings.TrimSpace(environment))
	if !validDiscoveryValue(applicationCode) || !validDiscoveryValue(environment) {
		return nil, provisioningError("service discovery target is invalid")
	}
	dockerBinary = strings.TrimSpace(dockerBinary)
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	ids, err := runDockerDiscoveryCommand(ctx, dockerBinary, "ps", "--filter", "label="+dockerApplicationLabel+"="+applicationCode, "--filter", "label="+dockerEnvironmentLabel+"="+environment, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("discover docker services: %w", err)
	}
	var services []application.SubsystemServiceInstance
	for _, id := range strings.Fields(ids) {
		labelsJSON, inspectErr := runDockerDiscoveryCommand(ctx, dockerBinary, "inspect", "--format", "{{json .Config.Labels}}", id)
		if inspectErr != nil {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(strings.TrimSpace(labelsJSON)), &labels) != nil {
			continue
		}
		health, _ := runDockerDiscoveryCommand(ctx, dockerBinary, "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", id)
		if service, ok := subsystemServiceInstanceFromDockerLabels(labels, strings.TrimSpace(health), time.Now()); ok {
			services = append(services, service)
		}
	}
	return services, nil
}

func runDockerDiscoveryCommand(ctx context.Context, dockerBinary string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, dockerBinary, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func subsystemServiceInstanceFromDockerLabels(labels map[string]string, health string, now time.Time) (application.SubsystemServiceInstance, bool) {
	get := func(key string) string { return strings.TrimSpace(labels[key]) }
	applicationCode := strings.ToLower(get(dockerApplicationLabel))
	environment := strings.ToLower(get(dockerEnvironmentLabel))
	host := get("com.basic-platform.internal_host")
	portText := get("com.basic-platform.internal_port")
	port, err := strconv.Atoi(portText)
	pathPrefix := strings.TrimRight(get("com.basic-platform.path_prefix"), "/")
	healthEndpoint := get("com.basic-platform.health_endpoint")
	if get(dockerDiscoveryLabel) != "v1" || !validDiscoveryValue(applicationCode) || !validDiscoveryValue(environment) || get("com.basic-platform.service_name") == "" || get("com.basic-platform.service_role") == "" || host == "" || hasControl(host) || err != nil || port < 1 || port > 65535 || !validOptionalDiscoveryPath(pathPrefix) || !validOptionalDiscoveryPath(healthEndpoint) {
		return application.SubsystemServiceInstance{}, false
	}
	protocol := strings.ToLower(get("com.basic-platform.protocol"))
	if protocol == "" {
		protocol = "http"
	}
	if protocol != "http" && protocol != "https" && protocol != "tcp" {
		return application.SubsystemServiceInstance{}, false
	}
	status := application.SubsystemServiceStatusDiscovered
	switch strings.ToLower(health) {
	case "healthy":
		status = application.SubsystemServiceStatusHealthy
	case "exited", "dead", "unhealthy", "created":
		status = application.SubsystemServiceStatusUnavailable
	}
	return application.SubsystemServiceInstance{
		// 数据库 ID 不属于容器声明。控制面在持久化前根据租户、应用编码和环境
		// 自然键补齐边界，Compose 无需硬编码平台 ULID。
		ApplicationCode: applicationCode, Environment: environment, ServiceName: get("com.basic-platform.service_name"), ServiceRole: get("com.basic-platform.service_role"),
		Protocol: protocol, InternalHost: host, InternalPort: uint(port), PathPrefix: pathPrefix, HealthEndpoint: healthEndpoint, Version: get("com.basic-platform.version"),
		Status: status, LastSeenAt: &now, CreatedAt: now, UpdatedAt: now,
	}, true
}

func subsystemDiscoveryCandidateFromDockerLabels(labels map[string]string, health string) (application.SubsystemDiscoveryCandidate, bool) {
	get := func(key string) string { return strings.TrimSpace(labels[key]) }
	applicationCode := strings.ToLower(get(dockerApplicationLabel))
	environment := strings.ToLower(get(dockerEnvironmentLabel))
	host := get("com.basic-platform.internal_host")
	port, err := strconv.Atoi(get("com.basic-platform.internal_port"))
	callbackPath := get("com.basic-platform.oidc_callback_path")
	if get(dockerDiscoveryLabel) != "v1" || !validDiscoveryValue(applicationCode) || !validDiscoveryValue(environment) || get("com.basic-platform.service_name") == "" || get("com.basic-platform.service_role") == "" || host == "" || hasControl(host) || err != nil || port < 1 || port > 65535 || !validOptionalDiscoveryPath(get("com.basic-platform.health_endpoint")) || !validOptionalDiscoveryPath(get("com.basic-platform.path_prefix")) || !validRequiredDiscoveryPath(callbackPath) {
		return application.SubsystemDiscoveryCandidate{}, false
	}
	protocol := strings.ToLower(get("com.basic-platform.protocol"))
	if protocol == "" {
		protocol = "http"
	}
	if protocol != "http" && protocol != "https" {
		return application.SubsystemDiscoveryCandidate{}, false
	}
	status := string(application.SubsystemServiceStatusDiscovered)
	switch strings.ToLower(health) {
	case "healthy":
		status = string(application.SubsystemServiceStatusHealthy)
	case "exited", "dead", "unhealthy", "created":
		status = string(application.SubsystemServiceStatusUnavailable)
	}
	return application.SubsystemDiscoveryCandidate{
		ApplicationCode: applicationCode, ApplicationName: firstNonEmpty(get("com.basic-platform.application_name"), get("com.basic-platform.service_name")), Environment: environment, ServiceName: get("com.basic-platform.service_name"), ServiceRole: get("com.basic-platform.service_role"),
		Protocol: protocol, InternalHost: host, InternalPort: uint(port), HealthEndpoint: get("com.basic-platform.health_endpoint"),
		OIDCCallbackPath: callbackPath, OIDCCallbackSupported: parseDiscoveryBool(get("com.basic-platform.oidc_callback_supported"), callbackPath != ""), Version: get("com.basic-platform.version"), Status: status,
	}, true
}

func validRequiredDiscoveryPath(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !hasControl(value)
}

func validOptionalDiscoveryPath(value string) bool {
	return value == "" || validRequiredDiscoveryPath(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func parseDiscoveryBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func validDiscoveryValue(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, r := range value {
		if !(unicode.IsLower(r) || unicode.IsDigit(r) || (index > 0 && (r == '-' || r == '_' || r == '.'))) {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
