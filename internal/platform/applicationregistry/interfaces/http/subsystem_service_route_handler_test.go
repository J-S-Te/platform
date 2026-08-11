package http

import (
	"context"
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
