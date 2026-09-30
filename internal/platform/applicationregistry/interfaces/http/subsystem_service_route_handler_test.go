package http

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type routeReaderStub struct{}

func (routeReaderStub) ListSubsystemServiceInstances(context.Context, string, string, string) ([]application.SubsystemServiceInstance, error) {
	return []application.SubsystemServiceInstance{{ApplicationCode: "orders", Environment: "prod", ServiceName: "api", ServiceRole: "business", Protocol: "http", InternalHost: "orders-api", InternalPort: 8080, Status: application.SubsystemServiceStatusHealthy}}, nil
}

type proxyRouteReader struct {
	host string
	port uint
}

func (reader proxyRouteReader) ListSubsystemServiceInstances(context.Context, string, string, string) ([]application.SubsystemServiceInstance, error) {
	return []application.SubsystemServiceInstance{{ServiceRole: "business", Protocol: "http", InternalHost: reader.host, InternalPort: reader.port, Status: application.SubsystemServiceStatusHealthy}}, nil
}

// routeAuthorizerStub 模拟逐应用授权复核结果（AUD-2026-012）。
type routeAuthorizerStub struct {
	granted bool
	err     error
}

func (stub routeAuthorizerStub) HasApplicationGrant(context.Context, string, string, string) (bool, error) {
	return stub.granted, stub.err
}

func newResolveTestRequest() *http.Request {
	request := httptest.NewRequest("GET", "/api/v1/subsystem-service-route?application_code=orders&environment=prod&service_role=business", nil)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
}

// 安全审查 AUD-2026-012：Resolve 与 Proxy 暴露同样的内网 UpstreamURL，仅有平台目录读
// 权限（未获得目标应用授权）的调用方必须被 403 拒绝，不能枚举任意子系统的内网地址。
func TestSubsystemServiceRouteResolveRejectsCallerWithoutApplicationGrant(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderStub{}, routeAuthorizerStub{granted: false})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Resolve(response, newResolveTestRequest())
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", response.Code, response.Body.String())
	}
}

func TestSubsystemServiceRouteResolveAllowsGrantedCallerAndReturnsUpstreamURL(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderStub{}, routeAuthorizerStub{granted: true})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Resolve(response, newResolveTestRequest())
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", response.Code, response.Body.String())
	}
	if got := response.Body.String(); !strings.Contains(got, `"upstream_url":"http://orders-api:8080"`) {
		t.Fatalf("response does not contain upstream URL: %s", got)
	}
}

func TestSubsystemServiceRouteResolveMapsAuthorizerFailureToInternalError(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderStub{}, routeAuthorizerStub{err: errors.New("grant lookup unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Resolve(response, newResolveTestRequest())
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 (授权复核失败不得放行)", response.Code, response.Body.String())
	}
}

func TestSubsystemServiceRouteHandlerResolvesHealthyRoute(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderStub{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/v1/subsystem-service-route?application_code=orders&environment=prod&service_role=business", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}))
	response := httptest.NewRecorder()
	handler.Resolve(response, request)
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); !strings.Contains(got, "http://orders-api:8080") {
		t.Fatalf("response does not contain upstream URL: %s", got)
	}
}

func TestSubsystemServiceRouteHandlerProxiesDiscoveredRoute(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health" || request.URL.Query().Get("source") != "portal" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, "proxied")
	}))
	backend.Listener = listener
	backend.Start()
	defer backend.Close()
	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewSubsystemServiceRouteHandler(proxyRouteReader{host: parsed.Hostname(), port: uint(port)})
	if err != nil {
		t.Fatal(err)
	}
	// 安全（SEC-B5）：回环目标默认被出网策略拒绝；本用例显式配置允许后才能代理成功，
	// 同一机制在部署侧对应 PLATFORM_EGRESS_ALLOW_LOOPBACK=true。
	handler.egress = application.NewEgressPolicy(true, false)
	request := httptest.NewRequest("GET", "/api/v1/subsystems/orders/health?environment=prod&service_role=business&source=portal", nil)
	request.SetPathValue("application_code", "orders")
	request.SetPathValue("path", "/health")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}))
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

// 安全审查 AUD-2026-011：上游子系统返回的 Set-Cookie 与 hop-by-hop 头必须被代理剥离，
// 防止被接管子系统对平台源做 cookie tossing；其余响应头（如 Content-Type、自定义追踪头）
// 原样保留。
func TestSubsystemServiceRouteProxyStripsUpstreamCookiesAndHopByHopHeaders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Add("Set-Cookie", "bp_session=attacker-value; Path=/")
		writer.Header().Add("Set-Cookie", "sub_tracking=tossed; Path=/")
		writer.Header().Set("Keep-Alive", "timeout=5")
		writer.Header().Set("Proxy-Instruction", "undermined")
		writer.Header().Set("X-Custom-Trace", "keepme")
		_, _ = io.WriteString(writer, "proxied")
	}))
	backend.Listener = listener
	backend.Start()
	defer backend.Close()
	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewSubsystemServiceRouteHandler(proxyRouteReader{host: parsed.Hostname(), port: uint(port)})
	if err != nil {
		t.Fatal(err)
	}
	handler.egress = application.NewEgressPolicy(true, false)
	request := httptest.NewRequest("GET", "/api/v1/subsystems/orders/health?environment=prod&service_role=business", nil)
	request.SetPathValue("application_code", "orders")
	request.SetPathValue("path", "/health")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}))
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if cookies := response.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("upstream Set-Cookie must be stripped (cookie tossing), got %v", cookies)
	}
	for _, name := range []string{"Keep-Alive", "Proxy-Instruction", "Connection", "Transfer-Encoding", "Upgrade"} {
		if values := response.Header().Values(name); len(values) != 0 {
			t.Fatalf("hop-by-hop header %s must be stripped, got %v", name, values)
		}
	}
	if response.Header().Get("X-Custom-Trace") != "keepme" || response.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("unrelated response headers must be preserved, got %v", response.Header())
	}
}

// stripSubsystemProxyResponseHeaders 纯函数断言：清单内头被删除、清单外头保留。
func TestStripSubsystemProxyResponseHeadersRemovesOnlyListedHeaders(t *testing.T) {
	header := http.Header{}
	for _, name := range subsystemProxyStrippedResponseHeaders {
		header.Set(name, "value")
	}
	header.Set("Content-Type", "application/json")
	header.Set("X-Custom-Trace", "keepme")
	stripSubsystemProxyResponseHeaders(header)
	for _, name := range subsystemProxyStrippedResponseHeaders {
		if values := header.Values(name); len(values) != 0 {
			t.Fatalf("header %s must be stripped, got %v", name, values)
		}
	}
	if header.Get("Content-Type") != "application/json" || header.Get("X-Custom-Trace") != "keepme" {
		t.Fatalf("unlisted headers must be preserved, got %v", header)
	}
}
