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
func (*refreshSessionRepositoryStub) RotateRefreshToken(context.Context, RotateRefreshTokenCommand, time.Time) (domain.TokenGrant, error) {
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

type authorizationCodeSessionRepositoryStub struct {
	*refreshSessionRepositoryStub
	code domain.AuthorizationCode
}

func (repository *authorizationCodeSessionRepositoryStub) FindAuthorizationCode(context.Context, [32]byte, time.Time) (domain.AuthorizationCode, error) {
	return repository.code, nil
}
