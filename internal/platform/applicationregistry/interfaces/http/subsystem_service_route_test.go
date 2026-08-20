package http

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type routeReaderGrantStub struct {
	instances []application.SubsystemServiceInstance
}

func (stub routeReaderGrantStub) ListSubsystemServiceInstances(context.Context, string, string, string) ([]application.SubsystemServiceInstance, error) {
	return stub.instances, nil
}

type accessCheckerStub struct{ granted bool }

func (stub accessCheckerStub) HasApplicationGrant(context.Context, string, string, string) (bool, error) {
	return stub.granted, nil
}

func proxyRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
}

func healthyInstance() application.SubsystemServiceInstance {
	return application.SubsystemServiceInstance{
		TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "dev",
		ServiceRole: "web", Protocol: "http", InternalHost: "127.0.0.1", InternalPort: 1,
		Status: application.SubsystemServiceStatusHealthy,
	}
}

func TestProxyRequiresApplicationGrant(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderGrantStub{instances: []application.SubsystemServiceInstance{healthyInstance()}}, accessCheckerStub{granted: false})
	if err != nil {
		t.Fatal(err)
	}
	request := proxyRequest(http.MethodGet, "/api/v1/subsystems/contract_management/data?environment=dev&service_role=web")
	request.SetPathValue("application_code", "contract_management")
	request.SetPathValue("path", "/data")
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestProxyRejectsNonReadMethods(t *testing.T) {
	handler, err := NewSubsystemServiceRouteHandler(routeReaderGrantStub{instances: []application.SubsystemServiceInstance{healthyInstance()}}, accessCheckerStub{granted: true})
	if err != nil {
		t.Fatal(err)
	}
	request := proxyRequest(http.MethodPost, "/api/v1/subsystems/contract_management/data?environment=dev&service_role=web")
	request.SetPathValue("application_code", "contract_management")
	request.SetPathValue("path", "/data")
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

func TestProxyForwardsReadRequestsWhenGranted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/data" {
			t.Errorf("backend path = %q, want /data", request.URL.Path)
		}
		for _, name := range sensitiveSubsystemProxyRequestHeaders {
			if value := request.Header.Get(name); value != "" {
				t.Errorf("backend received sensitive header %s=%q", name, value)
			}
		}
		if got := request.Header.Get("X-Request-ID"); got != "trace-1" {
			t.Errorf("backend X-Request-ID = %q, want trace-1", got)
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer backend.Close()
	instance := healthyInstance()
	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	instance.InternalHost, instance.InternalPort = host, uint(port)
	handler, err := NewSubsystemServiceRouteHandler(routeReaderGrantStub{instances: []application.SubsystemServiceInstance{instance}}, accessCheckerStub{granted: true})
	if err != nil {
		t.Fatal(err)
	}
	request := proxyRequest(http.MethodGet, "/api/v1/subsystems/contract_management/data?environment=dev&service_role=web")
	request.SetPathValue("application_code", "contract_management")
	request.SetPathValue("path", "/data")
	for _, name := range sensitiveSubsystemProxyRequestHeaders {
		request.Header.Set(name, "must-not-cross-trust-boundary")
	}
	request.Header.Set("X-Request-ID", "trace-1")
	response := httptest.NewRecorder()
	handler.Proxy(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
}
