package httptransport

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

func TestRouterLivenessUsesStandardEnvelope(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{
		AppName:     "basic-platform",
		CORSOrigins: []string{"http://localhost:5173"},
	}
	router := NewRouter(cfg, logger, &sql.DB{})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JSON response", got)
	}
	if got := response.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("X-Request-ID is empty")
	}
}

func TestRouterRejectsUnknownRouteWithStandardError(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{AppName: "basic-platform", CORSOrigins: []string{"http://localhost:5173"}}
	router := NewRouter(cfg, logger, &sql.DB{})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}
