package identityhttp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func TestCreateUserRejectsUnknownFields(t *testing.T) {
	handler, err := NewManagementHandler(&managementServiceFake{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new management handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"display_name":"张三","unsupported":true}`))
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant"}, User: authctx.ReferenceName{ID: "operator"}}))
	response := httptest.NewRecorder()
	handler.CreateUser(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != "PLATFORM_VALIDATION_ERROR" {
		t.Fatalf("error code = %q", body.Code)
	}
}

type managementServiceFake struct{ managementApplicationService }

func (*managementServiceFake) CreateUser(context.Context, application.UserCreateInput) (application.UserView, error) {
	return application.UserView{}, nil
}
