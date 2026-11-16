package licensehttp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type runtimeApplicationsStub struct {
	items       []RuntimeApplication
	err         error
	tenantID    string
	environment string
}

func (s *runtimeApplicationsStub) ListRuntimeApplications(_ context.Context, tenantID, environment string) ([]RuntimeApplication, error) {
	s.tenantID = tenantID
	s.environment = environment
	return s.items, s.err
}

func runtimeApplicationsRequest() *http.Request {
	r := requestWith("", "platform:license:read")
	r.URL.Path = "/api/v1/licenses/enforcement"
	return r
}

func TestRuntimeApplicationsReturnsDetectedCatalogBackedSystems(t *testing.T) {
	h, _ := NewRuntimeHandler(&runtimeStub{})
	h.ConfigureOverview(&overviewLicenseStub{}, "prod")
	lister := &runtimeApplicationsStub{items: []RuntimeApplication{
		{Code: "settlement", Name: "结算与开票管理", Environment: "prod"},
		{Code: "contract_management", Name: "合同管理", Environment: "prod"},
		{Code: "custom_app", Name: "自定义应用", Environment: "prod"},
		{Code: "contract_management", Name: "重复项", Environment: "prod"},
		{Code: "project_management", Name: "项目服务管理", Environment: "test"},
	}}
	h.ConfigureRuntimeApplications(lister)
	response := invoke(h.Applications, runtimeApplicationsRequest())
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("applications status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"code":"contract_management"`, `"code":"settlement"`, `"name":"合同管理"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("applications lost %s: %s", want, body)
		}
	}
	if strings.Contains(body, "custom_app") || strings.Contains(body, "重复项") || strings.Contains(body, "project_management") {
		t.Fatalf("applications leaked unsupported/duplicate/wrong-environment rows: %s", body)
	}
	if lister.tenantID != "tenant" || lister.environment != "prod" {
		t.Fatalf("unexpected scope tenant=%q env=%q", lister.tenantID, lister.environment)
	}
}

func TestRuntimeApplicationsFailsClosedWhenDetectionUnavailable(t *testing.T) {
	h, _ := NewRuntimeHandler(&runtimeStub{})
	h.ConfigureOverview(&overviewLicenseStub{}, "prod")
	if response := invoke(h.Applications, runtimeApplicationsRequest()); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured lister status=%d body=%s", response.Code, response.Body.String())
	}
	h.ConfigureRuntimeApplications(&runtimeApplicationsStub{err: errors.New("mysql dial tcp private-host")})
	response := invoke(h.Applications, runtimeApplicationsRequest())
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed lister status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "mysql") || strings.Contains(response.Body.String(), "private-host") {
		t.Fatalf("storage error leaked to client: %s", response.Body.String())
	}
}

func TestRuntimeApplicationsEmptyDetectionReturnsEmptyItems(t *testing.T) {
	h, _ := NewRuntimeHandler(&runtimeStub{})
	h.ConfigureOverview(&overviewLicenseStub{}, "prod")
	h.ConfigureRuntimeApplications(&runtimeApplicationsStub{})
	response := invoke(h.Applications, runtimeApplicationsRequest())
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatalf("empty applications status=%d body=%s", response.Code, response.Body.String())
	}
}
