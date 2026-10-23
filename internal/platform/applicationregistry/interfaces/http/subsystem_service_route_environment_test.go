package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type environmentRouteAuthorizer struct {
	legacyCalls, environmentCalls int
	err                           error
}

// dev 环境中的授权会使旧的应用级查询返回真，即使
// 请求的 prod 环境应当拒绝。保留这两个方法是为了在修改生产代码前
// 通过失败测试识别误用授权契约的情况。
func (a *environmentRouteAuthorizer) HasApplicationGrant(context.Context, string, string, string) (bool, error) {
	a.legacyCalls++
	return true, nil
}

func (a *environmentRouteAuthorizer) HasApplicationEnvironmentGrant(_ context.Context, tenantID, userID, applicationCode, environment string) (bool, error) {
	a.environmentCalls++
	return tenantID == "tenant-1" && userID == "user-1" && applicationCode == "orders" && environment == "dev", a.err
}

type environmentRouteReader struct {
	host  string
	port  uint
	calls int
}

func (r *environmentRouteReader) ListSubsystemServiceInstances(_ context.Context, tenantID, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	r.calls++
	if tenantID != "tenant-1" || applicationCode != "orders" {
		return nil, errors.New("unexpected discovery scope")
	}
	return []application.SubsystemServiceInstance{{
		TenantID: tenantID, ApplicationCode: applicationCode, Environment: environment,
		ServiceRole: "web", Protocol: "http", InternalHost: r.host, InternalPort: r.port,
		Status: application.SubsystemServiceStatusHealthy,
	}}, nil
}

func TestSubsystemRoutesRequireGrantForRequestedEnvironment(t *testing.T) {
	for _, endpoint := range []string{"resolve", "proxy_get", "proxy_head"} {
		for _, tc := range []struct {
			name, environment string
			authorizerErr     error
			want              int
		}{
			{"granted_dev", "dev", nil, http.StatusOK},
			{"other_environment", "prod", nil, http.StatusForbidden},
			{"lookup_failure", "dev", errors.New("authorization store unavailable"), http.StatusInternalServerError},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var backendCalls atomic.Int32
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					backendCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				}))
				defer backend.Close()
				upstream, err := url.Parse(backend.URL)
				if err != nil {
					t.Fatal(err)
				}
				port, err := strconv.Atoi(upstream.Port())
				if err != nil {
					t.Fatal(err)
				}
				reader := &environmentRouteReader{host: upstream.Hostname(), port: uint(port)}
				authorizer := &environmentRouteAuthorizer{err: tc.authorizerErr}
				handler, err := NewSubsystemServiceRouteHandler(reader, authorizer)
				if err != nil {
					t.Fatal(err)
				}
				handler.egress = application.NewEgressPolicy(true, false)
				method := http.MethodGet
				if endpoint == "proxy_head" {
					method = http.MethodHead
				}
				request := proxyRequest(method, "/api/v1/subsystems/orders/data?application_code=orders&environment="+tc.environment+"&service_role=web")
				request.SetPathValue("application_code", "orders")
				request.SetPathValue("path", "/data")
				response := httptest.NewRecorder()
				if endpoint == "resolve" {
					handler.Resolve(response, request)
				} else {
					handler.Proxy(response, request)
				}
				if response.Code != tc.want {
					t.Errorf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
				}
				if authorizer.legacyCalls != 0 || authorizer.environmentCalls != 1 {
					t.Errorf("authorization calls: application=%d environment=%d; want only one environment check", authorizer.legacyCalls, authorizer.environmentCalls)
				}
				wantReads, wantBackend := 0, 0
				if tc.want == http.StatusOK {
					wantReads = 1
					if endpoint != "resolve" {
						wantBackend = 1
					}
				}
				if reader.calls != wantReads || backendCalls.Load() != int32(wantBackend) {
					t.Errorf("discovery=%d backend=%d; want %d/%d", reader.calls, backendCalls.Load(), wantReads, wantBackend)
				}
			})
		}
	}
}
