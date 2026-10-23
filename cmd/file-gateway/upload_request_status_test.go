package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	filehttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
)

func TestUploadRequestResolutionRouteRequiresMachineAuthentication(t *testing.T) {
	h := routes(&filehttp.Handler{}, &filehttp.UploadV2Handler{}, tokenMiddleware{}, nil, nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/file-upload-requests/source-key", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("resolve route status=%d, want401", w.Code)
	}
}

func TestUploadRequestRetirementRouteRequiresMachineAuthentication(t *testing.T) {
	h := routes(&filehttp.Handler{}, &filehttp.UploadV2Handler{}, tokenMiddleware{}, nil, nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/file-upload-requests/source-key/retire", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("retire route status=%d, want401", w.Code)
	}
}
