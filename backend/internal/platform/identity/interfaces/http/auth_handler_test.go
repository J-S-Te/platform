package identityhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

func TestWriteApplicationErrorReturnsConflictForConcurrentSession(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	response := httptest.NewRecorder()

	handler.writeApplicationError(response, request, application.ConcurrentSessionError{
		TenantID: "tenant-1", UserID: "user-1", AccountID: "account-1", AccountName: "alice",
	})

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Code != "AUTH_CONCURRENT_SESSION" {
		t.Fatalf("code = %q, want AUTH_CONCURRENT_SESSION", envelope.Code)
	}
	if envelope.Message != "该账号已有有效会话；如原页面已关闭，可选择退出原会话并重新登录" {
		t.Fatalf("message = %q", envelope.Message)
	}
}

func TestWriteApplicationErrorReturnsConflictForWrappedConcurrentSession(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	response := httptest.NewRecorder()

	handler.writeApplicationError(response, request, errors.Join(errors.New("persist login session"), application.ErrConcurrentSession))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Code != "AUTH_CONCURRENT_SESSION" {
		t.Fatalf("code = %q, want AUTH_CONCURRENT_SESSION", envelope.Code)
	}
}
