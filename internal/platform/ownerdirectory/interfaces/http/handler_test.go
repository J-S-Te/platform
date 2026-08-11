package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

type directoryServiceStub struct {
	query     application.Query
	principal appctx.Principal
	page      domain.Page
	err       error
}

func (stub *directoryServiceStub) List(_ context.Context, principal appctx.Principal, query application.Query) (domain.Page, error) {
	stub.principal, stub.query = principal, query
	return stub.page, stub.err
}

func TestHandlerRequiresCompleteMachinePrincipal(t *testing.T) {
	handler := newTestHandler(t, &directoryServiceStub{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/internal/owner-directory", nil)
	response := httptest.NewRecorder()

	handler.List(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", response.Code, http.StatusUnauthorized)
	}
}

func TestHandlerRejectsInvalidPagination(t *testing.T) {
	handler := newTestHandler(t, &directoryServiceStub{})
	request := authenticatedRequest("/api/v1/internal/owner-directory?page=0")
	response := httptest.NewRecorder()

	handler.List(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want=%d", response.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandlerPassesMachineScopeAndReturnsMinimalProjection(t *testing.T) {
	service := &directoryServiceStub{page: domain.Page{
		Items: []domain.User{{
			ID: "oidc-sub-1", DisplayName: "负责人甲",
			Organizations: []domain.Organization{{ID: "org-1", Name: "华东区", IsPrimary: true}},
		}},
		Page: 1, PageSize: 20, Total: 1,
	}}
	handler := newTestHandler(t, service)
	request := authenticatedRequest("/api/v1/internal/owner-directory?keyword=%E8%B4%9F%E8%B4%A3&page=2&page_size=10")
	response := httptest.NewRecorder()

	handler.List(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.principal.ApplicationID != "app-crm" || service.principal.EnvironmentID != "env-prod" {
		t.Fatalf("principal=%+v", service.principal)
	}
	if service.query.Keyword != "负责" || service.query.Page != 2 || service.query.PageSize != 10 {
		t.Fatalf("query=%+v", service.query)
	}
	body := response.Body.String()
	for _, expected := range []string{"oidc-sub-1", "负责人甲", "org-1", "华东区"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response body %q does not contain %q", body, expected)
		}
	}
	for _, forbidden := range []string{"email", "mobile", "person_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response body unexpectedly exposes %q: %s", forbidden, body)
		}
	}
}

func newTestHandler(t *testing.T, service directoryService) *Handler {
	t.Helper()
	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func authenticatedRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	principal := appctx.Principal{
		OAuthClientID: "oauth-crm-directory", ClientID: "crm-directory", TenantID: "tenant-1",
		ApplicationID: "app-crm", ApplicationCode: "CRM", EnvironmentID: "env-prod", EnvironmentCode: "PROD",
		Scopes: map[string]struct{}{"owner_directory.read": {}},
	}
	return request.WithContext(appctx.WithPrincipal(request.Context(), principal))
}
