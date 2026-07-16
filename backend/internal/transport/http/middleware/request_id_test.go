package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
)

func TestRequestIDGeneratesULIDAndStoresContext(t *testing.T) {
	var contextRequestID string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextRequestID = requestctx.RequestID(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := response.Header().Get(requestIDHeader); !isULID(got) {
		t.Fatalf("response request ID = %q, want ULID", got)
	}
	if contextRequestID != response.Header().Get(requestIDHeader) {
		t.Fatalf("context request ID = %q, want response header value", contextRequestID)
	}
}

func TestRequestIDAcceptsValidCallerIdentifier(t *testing.T) {
	const suppliedID = "01J123456789ABCDEFGHJKMNPQ"
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(requestIDHeader, suppliedID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get(requestIDHeader); got != suppliedID {
		t.Fatalf("response request ID = %q, want %q", got, suppliedID)
	}
}
