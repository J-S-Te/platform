package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// 安全（SEC-B5）负向测试：反向代理与健康探针必须共用 EgressPolicy——
// 元数据地址在任何配置下被拒；回环/私网在未显式配置允许时被拒。

func runProxyWithUpstream(t *testing.T, policy *application.EgressPolicy, host string, port uint) *httptest.ResponseRecorder {
	t.Helper()
	handler, err := NewSubsystemServiceRouteHandler(proxyRouteReader{host: host, port: port})
	if err != nil {
		t.Fatal(err)
	}
	handler.egress = policy
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystems/orders/health?environment=prod&service_role=business", nil)
	request.SetPathValue("application_code", "orders")
	request.SetPathValue("path", "/health")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}))
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	return response
}

func TestProxyRejectsMetadataUpstreamEvenWithPermissiveConfig(t *testing.T) {
	// 即使显式允许回环与私网，云元数据地址也必须在入口被拒，且不发起任何出网连接。
	response := runProxyWithUpstream(t, application.NewEgressPolicy(true, true), "169.254.169.254", 80)
	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "PLATFORM_DEPENDENCY_UNAVAILABLE") {
		t.Fatalf("body = %s", body)
	}
}

func TestProxyRejectsLoopbackUpstreamWithoutExplicitConfig(t *testing.T) {
	response := runProxyWithUpstream(t, application.NewEgressPolicy(false, false), "127.0.0.1", 1)
	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestProxyRejectsPrivateUpstreamWithoutExplicitConfig(t *testing.T) {
	response := runProxyWithUpstream(t, application.NewEgressPolicy(false, false), "10.20.30.40", 8080)
	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

type egressProbeStateStore struct {
	recordingSubsystemDeploymentStateStore
	targets []application.SubsystemHealthTarget
}

func (store *egressProbeStateStore) ResolveSubsystemHealthTargets(context.Context, string) ([]application.SubsystemHealthTarget, error) {
	return store.targets, nil
}

// runEgressHealthDashboard 用给定策略执行健康看板并返回 contract_management/prod 的条目。
func runEgressHealthDashboard(t *testing.T, policy *application.EgressPolicy, healthURL string) SubsystemHealthEntry {
	t.Helper()
	store := &egressProbeStateStore{targets: []application.SubsystemHealthTarget{
		{ApplicationCode: "contract_management", Environment: "prod", HealthURL: healthURL},
	}}
	service := &stubSubsystemOnboardingService{portalItems: []application.PortalApplication{
		{ApplicationID: "app-1", Code: "contract_management", Environment: "prod", Name: "合同管理系统"},
	}}
	handler, err := NewSubsystemOnboardingHandler(
		service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://platform.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)), store,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.egress = policy

	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-health", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()
	handler.GetSubsystemHealthDashboard(response, request)
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data []SubsystemHealthEntry `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, entry := range envelope.Data {
		if entry.ApplicationCode == "contract_management" && entry.Environment == "prod" {
			return entry
		}
	}
	t.Fatalf("dashboard entry missing: %s", response.Body.String())
	return SubsystemHealthEntry{}
}

func TestHealthProbeBlockedForLoopbackWithoutExplicitConfig(t *testing.T) {
	backend := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, _ *stdhttp.Request) {
		writer.WriteHeader(stdhttp.StatusOK)
	}))
	defer backend.Close()

	// 未显式允许回环：探针被策略拦截（看板保持 UNVERIFIED），后端可达也必须为 false。
	entry := runEgressHealthDashboard(t, application.NewEgressPolicy(false, false), backend.URL)
	if entry.RuntimeOK || entry.RuntimeStatus != "UNVERIFIED" {
		t.Fatalf("entry = %+v, want RuntimeOK=false UNVERIFIED", entry)
	}
}

func TestHealthProbeAllowedForLoopbackWithExplicitConfig(t *testing.T) {
	backend := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, _ *stdhttp.Request) {
		writer.WriteHeader(stdhttp.StatusOK)
	}))
	defer backend.Close()

	// 显式配置允许回环后探针恢复——与反向代理共用同一开关语义。
	entry := runEgressHealthDashboard(t, application.NewEgressPolicy(true, false), backend.URL)
	if !entry.RuntimeOK || entry.RuntimeStatus != "READY" {
		t.Fatalf("entry = %+v, want RuntimeOK=true READY", entry)
	}
}

func TestHealthProbeBlockedForMetadataEvenWithPermissiveConfig(t *testing.T) {
	entry := runEgressHealthDashboard(t, application.NewEgressPolicy(true, true), "http://169.254.169.254/latest/meta-data/")
	if entry.RuntimeOK {
		t.Fatalf("entry = %+v, 元数据地址必须被探针策略拒绝", entry)
	}
}
