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

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

const (
	dockerApplicationLabel = "com.basic-platform.application_code"
	dockerEnvironmentLabel = "com.basic-platform.environment"
)

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
	if get("com.basic-platform.tenant_id") == "" || get("com.basic-platform.application_id") == "" || get("com.basic-platform.environment_id") == "" || !validDiscoveryValue(applicationCode) || !validDiscoveryValue(environment) || get("com.basic-platform.service_name") == "" || get("com.basic-platform.service_role") == "" || host == "" || hasControl(host) || err != nil || port < 1 || port > 65535 {
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
		TenantID: get("com.basic-platform.tenant_id"), ApplicationID: get("com.basic-platform.application_id"), EnvironmentID: get("com.basic-platform.environment_id"),
		ApplicationCode: applicationCode, Environment: environment, ServiceName: get("com.basic-platform.service_name"), ServiceRole: get("com.basic-platform.service_role"),
		Protocol: protocol, InternalHost: host, InternalPort: uint(port), PathPrefix: strings.TrimRight(get("com.basic-platform.path_prefix"), "/"), HealthEndpoint: get("com.basic-platform.health_endpoint"), Version: get("com.basic-platform.version"),
		Status: status, LastSeenAt: &now, CreatedAt: now, UpdatedAt: now,
	}, true
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
