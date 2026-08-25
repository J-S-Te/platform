package oidchttp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

func TestWriteAuthorizeServiceErrorMapsPasswordChangeGateToAccessDenied(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	writeAuthorizeServiceError(response, logger, application.ErrAccessDenied)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"access_denied"`) {
		t.Fatalf("response does not contain OAuth access_denied: %s", response.Body.String())
	}
}
