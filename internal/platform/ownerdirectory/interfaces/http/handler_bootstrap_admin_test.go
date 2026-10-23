package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type bootstrapOwnerDirectoryStub struct {
	err error
}

func (stub bootstrapOwnerDirectoryStub) List(context.Context, appctx.Principal, application.Query) (domain.Page, error) {
	return domain.Page{}, stub.err
}

func TestOwnerDirectoryReportsBootstrapAdministratorConfigurationAnomaly(t *testing.T) {
	handler, err := NewHandler(bootstrapOwnerDirectoryStub{err: fmt.Errorf("resolve: %w", bootstrapadmin.ErrConfiguration)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/internal/owner-directory", nil)
	request = request.WithContext(appctx.WithPrincipal(request.Context(), appctx.Principal{
		OAuthClientID: "client", ClientID: "client", TenantID: "tenant-1",
		ApplicationID: "app", ApplicationCode: "customer_and_opportunity",
		EnvironmentID: "env", EnvironmentCode: "prod",
		Scopes: map[string]struct{}{"owner_directory.read": {}},
	}))
	recorder := httptest.NewRecorder()

	handler.List(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Code != "IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID" {
		t.Fatalf("code = %q, want IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID", envelope.Code)
	}
}
