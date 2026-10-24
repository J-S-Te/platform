package http

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

func TestAuthorizationWriteErrorMapsBootstrapAdministratorProtection(t *testing.T) {
	handler := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "protected",
			err:        fmt.Errorf("update role binding: %w", bootstrapadmin.ErrProtected),
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
			request := httptest.NewRequest(http.MethodPatch, "/api/v1/role-bindings/binding-bootstrap", nil)
			recorder := httptest.NewRecorder()
			handler.writeError(recorder, request, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			var envelope httpresponse.ErrorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if envelope.Code != tc.wantCode || envelope.Message != tc.wantMsg {
				t.Fatalf("envelope = %#v, want code=%q message=%q", envelope, tc.wantCode, tc.wantMsg)
			}
		})
	}
}
