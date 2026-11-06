package identityhttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
)

type loginFailureService struct {
	applicationService
	err error
}

func (service loginFailureService) Login(context.Context, application.LoginInput) (application.SessionResult, error) {
	return application.SessionResult{}, service.err
}

type discardLifecycleAuditRecorder struct{}

func (discardLifecycleAuditRecorder) Ingest(context.Context, string, auditapplication.EventInput) (auditdomain.Receipt, error) {
	return auditdomain.Receipt{}, nil
}

func newLoginFailureTestHandler(registry *eventmetrics.Registry, err error) *Handler {
	return &Handler{
		service:       loginFailureService{err: err},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cookie:        cookieConfig{name: "bp_session"},
		auditRecorder: discardLifecycleAuditRecorder{},
		eventMetrics:  registry,
	}
}

// 登录失败在发生点按受约束的 reason 枚举计数：错误类型决定桶，任何未知失败折叠 other，
// 不允许出现账号/租户等标签或取值。
func TestLoginFailureMetricUsesFoldedReason(t *testing.T) {
	allReasons := []string{
		eventmetrics.LoginFailureInvalidCredentials,
		eventmetrics.LoginFailureAccountDisabled,
		eventmetrics.LoginFailureIdPError,
		eventmetrics.LoginFailureRateLimited,
		eventmetrics.LoginFailureSessionExpired,
		eventmetrics.LoginFailureOther,
	}
	for _, testCase := range []struct {
		name       string
		err        error
		wantReason string
		wantStatus int
	}{
		{
			name:       "wrong password",
			err:        application.LoginFailedError{TenantID: "tenant-1", AccountID: "account-1"},
			wantReason: eventmetrics.LoginFailureInvalidCredentials,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "ineligible account",
			err:        application.LoginFailedError{TenantID: "tenant-1", AccountID: "account-1", Ineligible: true},
			wantReason: eventmetrics.LoginFailureAccountDisabled,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown account",
			err:        application.ErrUnauthenticated,
			wantReason: eventmetrics.LoginFailureInvalidCredentials,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "concurrent session",
			err:        application.ConcurrentSessionError{TenantID: "tenant-1"},
			wantReason: eventmetrics.LoginFailureOther,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "unexpected dependency failure",
			err:        errors.New("find password login account: database is down"),
			wantReason: eventmetrics.LoginFailureOther,
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := eventmetrics.NewRegistry()
			handler := newLoginFailureTestHandler(registry, testCase.err)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
				strings.NewReader(`{"login_type":"password","account":"alice","password":"correct-horse-battery"}`))
			response := httptest.NewRecorder()

			handler.Login(response, request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, testCase.wantStatus, response.Body.String())
			}
			if got := registry.LoginFailureCount(testCase.wantReason); got != 1 {
				t.Fatalf("reason %q count = %d, want 1", testCase.wantReason, got)
			}
			for _, other := range allReasons {
				if other != testCase.wantReason && registry.LoginFailureCount(other) != 0 {
					t.Fatalf("reason %q must stay 0 for %s", other, testCase.name)
				}
			}
		})
	}
}

// 成功的密码登录不增加失败计数。
func TestLoginFailureMetricIgnoresSuccessfulLogin(t *testing.T) {
	registry := eventmetrics.NewRegistry()
	handler := newLoginFailureTestHandler(registry, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"login_type":"password","account":"alice","password":"correct-horse-battery"}`))
	response := httptest.NewRecorder()
	handler.Login(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	for _, reason := range []string{
		eventmetrics.LoginFailureInvalidCredentials,
		eventmetrics.LoginFailureAccountDisabled,
		eventmetrics.LoginFailureIdPError,
		eventmetrics.LoginFailureRateLimited,
		eventmetrics.LoginFailureSessionExpired,
		eventmetrics.LoginFailureOther,
	} {
		if got := registry.LoginFailureCount(reason); got != 0 {
			t.Fatalf("reason %q count = %d, want 0 on success", reason, got)
		}
	}
}
