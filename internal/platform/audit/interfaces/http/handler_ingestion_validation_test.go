package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

func TestValidateIngestionReturnsVerifiedCredentialFreeBinding(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/ingest/validate", nil)
	request = request.WithContext(appctx.WithPrincipal(request.Context(), appctx.Principal{
		OAuthClientID: "oauth-client-1", ClientID: "customer_and_opportunity-dev-audit-publisher",
		TenantID: "tenant-1", ApplicationID: "application-1", ApplicationCode: "customer_and_opportunity",
		EnvironmentID: "environment-1", EnvironmentCode: "dev", Scopes: map[string]struct{}{"audit.ingest": {}},
	}))
	response := httptest.NewRecorder()

	handler.ValidateIngestion(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, expected := range []string{
		`"application_code":"customer_and_opportunity"`,
		`"environment_code":"dev"`,
		`"client_id":"customer_and_opportunity-dev-audit-publisher"`,
		`"audit_ingest":true`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("response does not contain %s: %s", expected, response.Body.String())
		}
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "secret") || strings.Contains(strings.ToLower(response.Body.String()), "token") {
		t.Fatalf("validation response exposed credential material: %s", response.Body.String())
	}
}

func TestValidateIngestionRejectsMissingApplicationPrincipal(t *testing.T) {
	handler := &Handler{}
	response := httptest.NewRecorder()

	handler.ValidateIngestion(response, httptest.NewRequest(http.MethodGet, "/api/v1/audit/ingest/validate", nil))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
