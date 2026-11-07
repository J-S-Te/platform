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
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	sharedsecurity "github.com/J-S-Te/Basic-Platform/internal/shared/security"
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

// loginAccountRepositoryStub 提供一条固定的登录账号，使登录失败 reason 用例可以走真实
// 认证服务（application.Service.Login），而不是在 HTTP 边界伪造错误分类。
type loginAccountRepositoryStub struct {
	account identitydomain.LoginAccount
}

func (stub loginAccountRepositoryStub) FindLoginAccount(context.Context, string) (identitydomain.LoginAccount, error) {
	return stub.account, nil
}

func (stub loginAccountRepositoryStub) FindLoginAccountByIdentityID(context.Context, string) (identitydomain.LoginAccount, error) {
	return stub.account, nil
}

func (loginAccountRepositoryStub) RecordSuccessfulPasswordVerification(context.Context, identitydomain.LoginAccount, time.Time) error {
	return nil
}

func (loginAccountRepositoryStub) CreateSession(context.Context, identitydomain.LoginAccount, identitydomain.Session, time.Duration, bool) error {
	return nil
}

func (loginAccountRepositoryStub) FindPrincipalBySession(context.Context, string, time.Time, time.Duration) (identitydomain.Principal, error) {
	return identitydomain.Principal{}, application.ErrUnauthenticated
}

func (loginAccountRepositoryStub) RecordSessionInteraction(context.Context, string, time.Time, time.Duration) error {
	return nil
}

func (loginAccountRepositoryStub) RefreshSession(_ context.Context, _ string, now time.Time) (time.Time, error) {
	return now.Add(8 * time.Hour), nil
}

func (loginAccountRepositoryStub) RevokeSession(context.Context, string, time.Time, string) error {
	return nil
}

func (loginAccountRepositoryStub) RevokeAccountSessions(context.Context, string, string, time.Time, string) error {
	return nil
}

// fixedPasswordVerifier 用固定结果替代 Argon2id 校验，唯一目的是控制“密码是否匹配”。
type fixedPasswordVerifier struct{ matched bool }

func (verifier fixedPasswordVerifier) Verify(string, string, []byte, []byte) (bool, error) {
	return verifier.matched, nil
}

type loginTestTokenManager struct{}

func (loginTestTokenManager) Issue(sharedsecurity.TokenClaims) (string, error) {
	return "signed-session-token", nil
}

func (loginTestTokenManager) Verify(string, time.Time) (sharedsecurity.TokenClaims, error) {
	return sharedsecurity.TokenClaims{}, nil
}

type loginTestIDGenerator struct{}

func (loginTestIDGenerator) New(time.Time) (string, error) { return "session-1", nil }

type loginTestClock struct{ now time.Time }

func (clock loginTestClock) Now() time.Time { return clock.now }

type loginTestLoginSecurity struct{}

func (loginTestLoginSecurity) RecordFailedLogin(context.Context, securityapplication.LoginFailureInput) (securityapplication.LoginFailureResult, error) {
	return securityapplication.LoginFailureResult{}, nil
}

func (loginTestLoginSecurity) SessionIdleTimeout(context.Context, string) (time.Duration, error) {
	return time.Hour, nil
}

// expectedUnauthenticatedBody 是修复前后都不得变化的 401 响应体（逐字节比对）。
const expectedUnauthenticatedBody = "{\"code\":\"AUTH_UNAUTHENTICATED\",\"message\":\"账号或密码错误\",\"request_id\":\"\"}\n"

// newDisabledAccountLoginHandler 用真实认证服务装配一个“凭据状态停用”的账号登录处理器。
// passwordMatches 控制密码校验结果，从而覆盖同一个停用账号的两种请求事实。
func newDisabledAccountLoginHandler(t *testing.T, registry *eventmetrics.Registry, passwordMatches bool) *Handler {
	t.Helper()
	now := time.Date(2026, time.November, 6, 10, 0, 0, 0, time.UTC)
	account := identitydomain.LoginAccount{
		TenantID: "tenant-1", TenantStatus: identitydomain.StatusActive,
		UserID: "user-1", UserName: "Alice", UserStatus: identitydomain.StatusActive,
		AccountID: "account-1", AccountName: "alice", AccountStatus: identitydomain.StatusActive,
		PasswordHash: []byte("digest"), HashAlgorithm: "argon2id",
		CredentialStatus: identitydomain.StatusDisabled,
	}
	service, err := application.NewService(
		loginAccountRepositoryStub{account: account}, fixedPasswordVerifier{matched: passwordMatches},
		loginTestTokenManager{}, loginTestIDGenerator{}, loginTestClock{now: now}, loginTestLoginSecurity{}, 8*time.Hour,
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return &Handler{
		service:       service,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cookie:        cookieConfig{name: "bp_session"},
		auditRecorder: discardLifecycleAuditRecorder{},
		eventMetrics:  registry,
	}
}

func newDisabledAccountLoginRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"login_type":"password","account":"alice","password":"correct-horse-battery"}`))
}

// 停用账号 + 错密码必须记为 invalid_credentials：认证服务在密码比对之前就判定“不可登录”，
// 若直接把该判定翻成 account_disabled，攻击者不用知道密码就能确认账号存在且被停用
// （reason 枚举侧信道）。响应体仍是同一个通用 401，逐字节不变。
func TestLoginFailureMetricHidesDisabledAccountWhenPasswordIsWrong(t *testing.T) {
	registry := eventmetrics.NewRegistry()
	handler := newDisabledAccountLoginHandler(t, registry, false)
	response := httptest.NewRecorder()

	handler.Login(response, newDisabledAccountLoginRequest())

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", response.Code, response.Body.String())
	}
	if got := registry.LoginFailureCount(eventmetrics.LoginFailureInvalidCredentials); got != 1 {
		t.Fatalf("invalid_credentials count = %d, want 1 (停用账号 + 错密码不得被判成 account_disabled)", got)
	}
	if got := registry.LoginFailureCount(eventmetrics.LoginFailureAccountDisabled); got != 0 {
		t.Fatalf("account_disabled count = %d, want 0 (否则 reason 成为账号状态枚举侧信道)", got)
	}
	if response.Body.String() != expectedUnauthenticatedBody {
		t.Fatalf("401 body = %q, want %q (对外文案不得随 reason 分类变化)", response.Body.String(), expectedUnauthenticatedBody)
	}
}

// 停用账号 + 正确密码才允许记 account_disabled：此时“凭据正确但身份不可用”已经是
// 服务端可确证的事实，不构成对未持有密码者的信息泄漏。
func TestLoginFailureMetricReportsDisabledAccountOnlyWithValidPassword(t *testing.T) {
	registry := eventmetrics.NewRegistry()
	handler := newDisabledAccountLoginHandler(t, registry, true)
	response := httptest.NewRecorder()

	handler.Login(response, newDisabledAccountLoginRequest())

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", response.Code, response.Body.String())
	}
	if got := registry.LoginFailureCount(eventmetrics.LoginFailureAccountDisabled); got != 1 {
		t.Fatalf("account_disabled count = %d, want 1", got)
	}
	if got := registry.LoginFailureCount(eventmetrics.LoginFailureInvalidCredentials); got != 0 {
		t.Fatalf("invalid_credentials count = %d, want 0 (凭据正确)", got)
	}
	if response.Body.String() != expectedUnauthenticatedBody {
		t.Fatalf("401 body = %q, want %q (对外文案不得随 reason 分类变化)", response.Body.String(), expectedUnauthenticatedBody)
	}
}
