package oidchttp

import (
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
)

func TestWriteOIDCTokenErrorMapsAccessDeniedToOAuthError(t *testing.T) {
	t.Parallel()
	handler := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	response := httptest.NewRecorder()

	handler.writeOIDCTokenError(response, fmt.Errorf("resolve application authorization: %w", application.ErrAccessDenied))

	if response.Code != stdhttp.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"access_denied"`) {
		t.Fatalf("response does not contain OAuth access_denied: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "role") || strings.Contains(response.Body.String(), "permission") {
		t.Fatalf("response leaked authorization internals: %s", response.Body.String())
	}
}
