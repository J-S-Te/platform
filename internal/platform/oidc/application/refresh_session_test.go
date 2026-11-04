package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

type refreshSessionRepositoryStub struct {
	client       domain.Client
	token        domain.RefreshToken
	subject      domain.SessionSubject
	subjectErr   error
	subjectCalls int
	rotateCalls  int
}

func (repository *refreshSessionRepositoryStub) FindClient(context.Context, string, time.Time) (domain.Client, error) {
	return repository.client, nil
}
func (repository *refreshSessionRepositoryStub) ResolveSessionSubject(context.Context, string, time.Time) (domain.SessionSubject, error) {
	repository.subjectCalls++
	return repository.subject, repository.subjectErr
}
func (*refreshSessionRepositoryStub) CreateAuthorizationCode(context.Context, domain.AuthorizationCode) error {
	return nil
}
func (*refreshSessionRepositoryStub) FindAuthorizationCode(context.Context, [32]byte, time.Time) (domain.AuthorizationCode, error) {
	return domain.AuthorizationCode{}, ErrNotFound
}
func (*refreshSessionRepositoryStub) ConsumeAuthorizationCode(context.Context, ConsumeAuthorizationCodeCommand, time.Time) (domain.TokenGrant, error) {
	return domain.TokenGrant{}, ErrInvalidGrant
}
func (repository *refreshSessionRepositoryStub) FindRefreshToken(context.Context, [32]byte, time.Time) (domain.RefreshToken, error) {
	return repository.token, nil
}
func (repository *refreshSessionRepositoryStub) RotateRefreshToken(context.Context, RotateRefreshTokenCommand, time.Time) (domain.TokenGrant, error) {
	repository.rotateCalls++
	return domain.TokenGrant{}, errors.New("rotation must not run for an invalid browser session")
}
func (*refreshSessionRepositoryStub) RevokeToken(context.Context, RevokeTokenCommand, time.Time) error {
	return nil
}
func (*refreshSessionRepositoryStub) IsTokenRevoked(context.Context, string, [32]byte, time.Time) (bool, error) {
	return false, nil
}
func (*refreshSessionRepositoryStub) ResolveUserInfo(context.Context, UserInfoQuery, time.Time) (domain.UserInfoSubject, error) {
	return domain.UserInfoSubject{}, ErrNotFound
}

type refreshSessionClock struct{ now time.Time }

func (clock refreshSessionClock) Now() time.Time { return clock.now }

func TestRefreshRejectsTokenWhoseBrowserSessionWasRevoked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	repository := &refreshSessionRepositoryStub{
		client: domain.Client{
			ID: "oauth-client-1", TenantID: "tenant-1", ClientID: "contract-web",
			ClientType: "public", TokenAuthMethod: "none",
			GrantTypes: map[string]struct{}{"refresh_token": {}},
		},
		token: domain.RefreshToken{
			ID: "refresh-1", TenantID: "tenant-1", OAuthClientID: "oauth-client-1",
			SessionID: "session-old-user", AccountID: "account-old-user", UserID: "user-old",
			Status: domain.RefreshTokenStatusActive, ExpiresAt: now.Add(time.Hour),
		},
		subjectErr: ErrNotFound,
	}
	service := &Service{repository: repository, clock: refreshSessionClock{now: now}}

	_, err := service.Refresh(context.Background(), RefreshTokenInput{
		ClientAuthentication: ClientAuthentication{ClientID: "contract-web"},
		RefreshToken:         "old-user-refresh-token",
	})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("Refresh() error = %v, want ErrInvalidGrant", err)
	}
	if repository.subjectCalls != 1 {
		t.Fatalf("ResolveSessionSubject calls = %d, want 1", repository.subjectCalls)
	}
}

// 安全审计 M-1：浏览器会话空闲超过租户策略窗口时，共享会话门禁
// （ResolveSessionSubject 的 sec_login_policy 空闲谓词）返回未找到，刷新令牌流程必须
// 在轮换前终止。仓储层 SQL 语义见 oidc/infrastructure 的 M-1 用例；这里锁定“拒绝且不
// 发生 rotate”，避免修复被回退成只拒绝响应而仍换出新令牌。
func TestRefreshRejectsIdleBrowserSessionWithoutRotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	repository := &refreshSessionRepositoryStub{
		client: domain.Client{
			ID: "oauth-client-1", TenantID: "tenant-1", ClientID: "contract-web",
			ClientType: "public", TokenAuthMethod: "none",
			GrantTypes: map[string]struct{}{"refresh_token": {}},
		},
		token: domain.RefreshToken{
			ID: "refresh-1", TenantID: "tenant-1", OAuthClientID: "oauth-client-1",
			SessionID: "session-idle", AccountID: "account-1", UserID: "user-1",
			Status: domain.RefreshTokenStatusActive, ExpiresAt: now.Add(time.Hour),
		},
		// 空闲超窗：仓储的空闲谓词不再返回该会话，等价于身份侧 ErrUnauthenticated。
		subjectErr: ErrNotFound,
	}
	service := &Service{repository: repository, clock: refreshSessionClock{now: now}}

	_, err := service.Refresh(context.Background(), RefreshTokenInput{
		ClientAuthentication: ClientAuthentication{ClientID: "contract-web"},
		RefreshToken:         "idle-session-refresh-token",
	})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("Refresh() error = %v, want ErrInvalidGrant", err)
	}
	if repository.subjectCalls != 1 {
		t.Fatalf("ResolveSessionSubject calls = %d, want 1", repository.subjectCalls)
	}
	if repository.rotateCalls != 0 {
		t.Fatalf("RotateRefreshToken calls = %d, want 0 for an idle session", repository.rotateCalls)
	}
}

func TestAuthorizationCodeExchangeRejectsCodeWhoseBrowserSessionWasRevoked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	repository := &refreshSessionRepositoryStub{
		client: domain.Client{
			ID: "oauth-client-1", TenantID: "tenant-1", ClientID: "contract-web",
			ClientType: "public", TokenAuthMethod: "none",
			GrantTypes: map[string]struct{}{"authorization_code": {}},
		},
		subjectErr: ErrNotFound,
	}
	repositoryAuthorizationCode := domain.AuthorizationCode{
		TenantID: "tenant-1", OAuthClientID: "oauth-client-1", SessionID: "session-old-user",
		AccountID: "account-old-user", UserID: "user-old", RedirectURI: "http://localhost/callback",
		Status: domain.AuthorizationCodeStatusActive, ExpiresAt: now.Add(time.Minute),
	}
	repository.token = domain.RefreshToken{}
	service := &Service{repository: &authorizationCodeSessionRepositoryStub{
		refreshSessionRepositoryStub: repository,
		code:                         repositoryAuthorizationCode,
	}, clock: refreshSessionClock{now: now}}

	_, err := service.ExchangeAuthorizationCode(context.Background(), AuthorizationCodeExchangeInput{
		ClientAuthentication: ClientAuthentication{ClientID: "contract-web"},
		Code:                 "authorization-code-for-old-user",
		RedirectURI:          "http://localhost/callback",
	})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("ExchangeAuthorizationCode() error = %v, want ErrInvalidGrant", err)
	}
}

func TestAuthorizeRejectsSessionThatMustChangePassword(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	repository := &refreshSessionRepositoryStub{
		client: domain.Client{
			ID: "oauth-client-1", TenantID: "tenant-1", ClientID: "customer-portal",
			GrantTypes:   map[string]struct{}{"authorization_code": {}},
			Scopes:       map[string]struct{}{"openid": {}},
			RedirectURIs: map[string]struct{}{"https://portal.example.com/callback": {}},
		},
		subject: domain.SessionSubject{
			TenantID: "tenant-1", SessionID: "session-1", AccountID: "account-1", UserID: "user-1",
			ExpiresAt: now.Add(time.Hour), MustChangePassword: true,
		},
	}
	service := &Service{repository: repository, clock: refreshSessionClock{now: now}}

	_, err := service.Authorize(context.Background(), AuthorizationInput{
		ClientID: "customer-portal", RedirectURI: "https://portal.example.com/callback",
		Scopes: []string{"openid"}, SessionID: "session-1",
	})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Authorize() error = %v, want ErrAccessDenied", err)
	}
}

func TestGrantSessionRejectsAccountThatMustChangePassword(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	repository := &refreshSessionRepositoryStub{subject: domain.SessionSubject{
		TenantID: "tenant-1", SessionID: "session-1", AccountID: "account-1", UserID: "user-1",
		ExpiresAt: now.Add(time.Hour), MustChangePassword: true,
	}}
	service := &Service{repository: repository, clock: refreshSessionClock{now: now}}

	err := service.validateGrantSession(context.Background(), "tenant-1", "session-1", "account-1", "user-1", now)
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("validateGrantSession() error = %v, want ErrInvalidGrant", err)
	}
}

type authorizationCodeSessionRepositoryStub struct {
	*refreshSessionRepositoryStub
	code domain.AuthorizationCode
}

func (repository *authorizationCodeSessionRepositoryStub) FindAuthorizationCode(context.Context, [32]byte, time.Time) (domain.AuthorizationCode, error) {
	return repository.code, nil
}
