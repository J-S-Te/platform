package infrastructure

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestSubsystemServiceInstanceFromDockerLabels(t *testing.T) {
	now := time.Now().UTC()
	service, ok := subsystemServiceInstanceFromDockerLabels(map[string]string{
		"com.basic-platform.tenant_id": "tenant-1", "com.basic-platform.application_id": "app-1", "com.basic-platform.environment_id": "env-1",
		"com.basic-platform.application_code": "orders", "com.basic-platform.environment": "prod", "com.basic-platform.service_name": "api", "com.basic-platform.service_role": "business",
		"com.basic-platform.internal_host": "orders-api", "com.basic-platform.internal_port": "8080", "com.basic-platform.protocol": "http", "com.basic-platform.path_prefix": "/orders/", "com.basic-platform.version": "1.2.3",
	}, "healthy", now)
	if !ok {
		t.Fatal("expected valid labels")
	}
	if service.Status != application.SubsystemServiceStatusHealthy || service.InternalPort != 8080 || service.PathPrefix != "/orders" {
		t.Fatalf("unexpected service: %+v", service)
	}
	if service.LastSeenAt == nil || !service.LastSeenAt.Equal(now) {
		t.Fatalf("expected last seen timestamp %v, got %v", now, service.LastSeenAt)
	}
}

func TestSubsystemServiceInstanceFromDockerLabelsRejectsInvalidPortAndTarget(t *testing.T) {
	labels := map[string]string{
		"com.basic-platform.tenant_id": "tenant-1", "com.basic-platform.application_id": "app-1", "com.basic-platform.environment_id": "env-1",
		"com.basic-platform.application_code": "orders", "com.basic-platform.environment": "prod", "com.basic-platform.service_name": "api", "com.basic-platform.service_role": "business",
		"com.basic-platform.internal_host": "orders-api", "com.basic-platform.internal_port": "70000",
	}
	if _, ok := subsystemServiceInstanceFromDockerLabels(labels, "running", time.Now()); ok {
		t.Fatal("expected invalid port to be rejected")
	}
	labels["com.basic-platform.internal_port"] = "8080"
	labels["com.basic-platform.application_code"] = "other app"
	if _, ok := subsystemServiceInstanceFromDockerLabels(labels, "running", time.Now()); ok {
		t.Fatal("expected invalid application code to be rejected")
	}
}

func TestSubsystemDiscoveryCandidateIncludesGenericApplicationAndOIDCMetadata(t *testing.T) {
	candidate, ok := subsystemDiscoveryCandidateFromDockerLabels(map[string]string{
		"com.basic-platform.application_code":        "inventory",
		"com.basic-platform.application_name":        "库存管理系统",
		"com.basic-platform.environment":             "dev",
		"com.basic-platform.service_name":            "inventory-api",
		"com.basic-platform.service_role":            "business",
		"com.basic-platform.internal_host":           "inventory-api",
		"com.basic-platform.internal_port":           "8080",
		"com.basic-platform.health_endpoint":         "/healthz",
		"com.basic-platform.oidc_callback_path":      "/auth/callback",
		"com.basic-platform.oidc_callback_supported": "true",
		"com.basic-platform.version":                 "1.2.3",
	}, "healthy")
	if !ok {
		t.Fatal("expected valid generic discovery candidate")
	}
	if candidate.ApplicationName != "库存管理系统" || !candidate.OIDCCallbackSupported || candidate.HealthEndpoint != "/healthz" || candidate.Version != "1.2.3" {
		t.Fatalf("candidate metadata = %+v", candidate)
	}
}
