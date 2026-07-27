// Package infrastructure contains production adapters for the external OIDC browser-login flow.
package infrastructure

import (
	"context"
	"errors"
	"net"
	"strings"

	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	loginapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

var (
	// ErrInvalidFederatedLoginResult prevents an incomplete or contradictory identity-service
	// response from being treated as an authenticated browser outcome.
	ErrInvalidFederatedLoginResult = errors.New("invalid federated login result")
)

// FederatedLoginUseCase is the trusted identity-application operation used after the external
// provider callback has verified its ID token and resolved a local account binding. It deliberately
// accepts only local identifiers and client metadata; it never receives upstream credentials.
type FederatedLoginUseCase interface {
	LoginFederated(context.Context, identityapplication.FederatedLoginInput) (identityapplication.SessionResult, error)
}

// FederatedSessionIssuer adapts a verified external identity into identityapplication.Service.
type FederatedSessionIssuer struct {
	authentication FederatedLoginUseCase
}

// NewFederatedSessionIssuer constructs a session issuer backed by the unified identity
// authentication service.
func NewFederatedSessionIssuer(authentication FederatedLoginUseCase) (*FederatedSessionIssuer, error) {
	if authentication == nil {
		return nil, errors.New("federated login use case must not be nil")
	}
	return &FederatedSessionIssuer{authentication: authentication}, nil
}

// IssueBrowserSession maps a verified local federation identity to identityapplication.Service.
func (issuer *FederatedSessionIssuer) IssueBrowserSession(ctx context.Context, issue domain.SessionIssue) (domain.BrowserSession, error) {
	if issuer == nil || issuer.authentication == nil {
		return domain.BrowserSession{}, errors.New("federated login use case must not be nil")
	}

	input, err := federatedLoginInputFromIssue(issue)
	if err != nil {
		return domain.BrowserSession{}, err
	}
	result, err := issuer.authentication.LoginFederated(ctx, input)
	if err != nil {
		return domain.BrowserSession{}, err
	}

	session, err := browserSessionFromLoginResult(result)
	if err != nil {
		return domain.BrowserSession{}, err
	}
	return session, nil
}

func federatedLoginInputFromIssue(issue domain.SessionIssue) (identityapplication.FederatedLoginInput, error) {
	tenantID := strings.TrimSpace(issue.TenantID)
	userID := strings.TrimSpace(issue.UserID)
	accountID := strings.TrimSpace(issue.AccountID)
	if tenantID == "" || userID == "" || accountID == "" {
		return identityapplication.FederatedLoginInput{}, ErrInvalidFederatedLoginResult
	}

	return identityapplication.FederatedLoginInput{
		TenantID:  tenantID,
		UserID:    userID,
		AccountID: accountID,
		IPAddress: net.IP(append([]byte(nil), issue.IPAddress...)),
		UserAgent: issue.UserAgent,
	}, nil
}

func browserSessionFromLoginResult(result identityapplication.SessionResult) (domain.BrowserSession, error) {
	if strings.TrimSpace(result.Token) == "" || result.ExpiresAt.IsZero() {
		return domain.BrowserSession{}, ErrInvalidFederatedLoginResult
	}
	return domain.BrowserSession{CookieValue: result.Token, ExpiresAt: result.ExpiresAt}, nil
}

var _ loginapplication.SessionIssuer = (*FederatedSessionIssuer)(nil)
