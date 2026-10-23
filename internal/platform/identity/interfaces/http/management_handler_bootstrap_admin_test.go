package identityhttp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

func bootstrapAdminTestHandler(t *testing.T) *ManagementHandler {
	t.Helper()
	handler, err := NewManagementHandler(
		&auditResourceServiceStub{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowAllScopeAuthorizer{},
	)
	if err != nil {
		t.Fatalf("NewManagementHandler() error = %v", err)
	}
	return handler
}

func decodeErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) httpresponse.ErrorEnvelope {
	t.Helper()
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, recorder.Body.String())
	}
	return envelope
}

func TestManagementWriteErrorMapsBootstrapAdministratorProtection(t *testing.T) {
	handler := bootstrapAdminTestHandler(t)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "protected",
			err:        fmt.Errorf("delete user: %w", bootstrapadmin.ErrProtected),
			wantStatus: http.StatusConflict,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_PROTECTED",
			wantMsg:    "部署初始超级管理员为系统保留账号，不允许执行此操作。",
		},
		{
			name:       "configuration anomaly",
			err:        fmt.Errorf("resolve bootstrap administrator: %w", bootstrapadmin.ErrConfiguration),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID",
			wantMsg:    "部署初始超级管理员初始化记录异常，请联系运维处理",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodDelete, "/api/v1/users/user-bootstrap", nil)
			recorder := httptest.NewRecorder()
			handler.writeError(recorder, request, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder)
			if envelope.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
			if envelope.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", envelope.Message, tc.wantMsg)
			}
		})
	}
}

func TestManagementWriteErrorKeepsOrdinaryConflicts(t *testing.T) {
	handler := bootstrapAdminTestHandler(t)
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/users/user-ordinary", nil)
	recorder := httptest.NewRecorder()

	handler.writeError(recorder, request, fmt.Errorf("ordinary: %w", application.ErrConflict))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if envelope := decodeErrorEnvelope(t, recorder); envelope.Code != "IAM_CONFLICT" {
		t.Fatalf("code = %q, want IAM_CONFLICT", envelope.Code)
	}
}
