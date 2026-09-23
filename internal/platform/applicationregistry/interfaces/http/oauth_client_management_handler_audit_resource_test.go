package http

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// 安全审查 SEC-D5：oauth-clients 创建接口必须把被创建对象 id 登记给审计中间件槽位
// （task-38 拆分出的 applicationregistry 侧接线，task-55 归属）。

type oauthAuditResourceServiceStub struct {
	oauthClientManagementService
	createErr error
}

func (stub *oauthAuditResourceServiceStub) CreateOAuthClient(context.Context, application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error) {
	if stub.createErr != nil {
		return application.OAuthClientCreateResult{}, stub.createErr
	}
	return application.OAuthClientCreateResult{Client: application.OAuthClientView{ID: "ocl_created_1", ClientID: "audit-client"}}, nil
}

func newOAuthAuditResourceHandler(t *testing.T, stub *oauthAuditResourceServiceStub) *OAuthClientManagementHandler {
	t.Helper()
	handler, err := NewOAuthClientManagementHandler(stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewOAuthClientManagementHandler() error = %v", err)
	}
	return handler
}

func oauthAuditResourceRequest(payload string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth-clients", bytes.NewBufferString(payload))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(middleware.AttachAuditResourceTarget(request.Context()))
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
}

func TestCreateOAuthClientRegistersAuditResourceID(t *testing.T) {
	handler := newOAuthAuditResourceHandler(t, &oauthAuditResourceServiceStub{})
	request := oauthAuditResourceRequest(`{"application_id":"app-1","environment_id":"env-1","client_id":"audit-client","client_name":"审计客户端","client_type":"confidential","token_auth_method":"client_secret_basic","access_token_ttl_seconds":900,"refresh_token_ttl_seconds":86400,"require_pkce":true,"grant_types":["authorization_code"],"scopes":["openid"],"redirect_uris":["https://app.example.com/cb"]}`)
	recorder := httptest.NewRecorder()

	handler.CreateOAuthClient(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "ocl_created_1" {
		t.Fatalf("audit resource id = %q, want ocl_created_1", got)
	}
}

func TestCreateOAuthClientFailureRegistersNoAuditResourceID(t *testing.T) {
	handler := newOAuthAuditResourceHandler(t, &oauthAuditResourceServiceStub{createErr: application.ErrManagementConflict})
	request := oauthAuditResourceRequest(`{"application_id":"app-1","environment_id":"env-1","client_id":"audit-client","client_name":"审计客户端"}`)
	recorder := httptest.NewRecorder()

	handler.CreateOAuthClient(recorder, request)

	if recorder.Code < 400 {
		t.Fatalf("status = %d, want error response", recorder.Code)
	}
	if got := middleware.AuditResourceIDFromRequest(request); got != "" {
		t.Fatalf("创建失败不得登记 audit resource id, got %q", got)
	}
}
