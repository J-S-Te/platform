// Package application implements OAuth Client Credentials issuance and bearer authentication.
package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrUnauthenticated intentionally covers unknown clients and invalid secrets so the token
	// endpoint does not disclose whether a client identifier is registered.
	ErrUnauthenticated = errors.New("application client authentication failed")
	ErrInvalidGrant    = errors.New("unsupported OAuth grant")
	ErrInvalidScope    = errors.New("requested OAuth scope is not allowed")
)

type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Repository provides the two registration reads needed by the token lifecycle.
type Repository interface {
	FindForClientCredentials(context.Context, string, time.Time) (domain.OAuthClient, []domain.ClientCredential, error)
	FindActiveByID(context.Context, string, time.Time) (domain.OAuthClient, error)
}

// TokenManager isolates the Ed25519 JWT implementation from the OAuth application service.
type TokenManager interface {
	Issue(security.ApplicationTokenClaims) (string, error)
	Verify(string, time.Time) (security.ApplicationTokenClaims, error)
}

type TokenResult struct {
	AccessToken string
	ExpiresIn   int64
	Scope       string
}

// Service issues short-lived application tokens and validates their current database-backed
// client registration. The latter re-check means client suspension/revocation takes effect
// without Redis or an in-process denylist.
type Service struct {
	repository Repository
	tokens     TokenManager
	clock      Clock
}

func NewService(repository Repository, tokens TokenManager, clock Clock) (*Service, error) {
	if repository == nil || tokens == nil || clock == nil {
		return nil, errors.New("application registry service dependencies must not be nil")
	}
	return &Service{repository: repository, tokens: tokens, clock: clock}, nil
}

// IssueClientCredentials validates HTTP Basic client authentication and creates an access token.
func (s *Service) IssueClientCredentials(ctx context.Context, clientID, clientSecret string, requestedScopes []string) (TokenResult, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientSecret == "" {
		return TokenResult{}, ErrUnauthenticated
	}

	now := s.clock.Now().UTC()
	client, credentials, err := s.repository.FindForClientCredentials(ctx, clientID, now)
	if err != nil {
		return TokenResult{}, mapAuthenticationError(err)
	}
	if client.TokenAuthMethod != "client_secret_basic" || !has(client.GrantTypes, "client_credentials") {
		return TokenResult{}, ErrInvalidGrant
	}
	if !verifyAnySecret(credentials, clientSecret) {
		return TokenResult{}, ErrUnauthenticated
	}

	scopes, err := allowedScopes(client.Scopes, requestedScopes)
	if err != nil {
		return TokenResult{}, err
	}
	if client.AccessTokenTTLSeconds == 0 {
		return TokenResult{}, fmt.Errorf("application client %q has an invalid access token TTL", client.ClientID)
	}

	ttl := time.Duration(client.AccessTokenTTLSeconds) * time.Second
	token, err := s.tokens.Issue(security.ApplicationTokenClaims{
		OAuthClientID:   client.ID,
		ClientID:        client.ClientID,
		TenantID:        client.TenantID,
		ApplicationID:   client.ApplicationID,
		ApplicationCode: client.ApplicationCode,
		EnvironmentID:   client.EnvironmentID,
		EnvironmentCode: client.EnvironmentCode,
		Scopes:          scopes,
		IssuedAt:        now,
		ExpiresAt:       now.Add(ttl),
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue application access token: %w", err)
	}
	return TokenResult{AccessToken: token, ExpiresIn: int64(ttl / time.Second), Scope: strings.Join(scopes, " ")}, nil
}

// Authenticate validates a bearer token and confirms the referenced application client remains
// active and bound to the same tenant/application/environment.
func (s *Service) Authenticate(ctx context.Context, token string) (appctx.Principal, error) {
	claims, err := s.tokens.Verify(strings.TrimSpace(token), s.clock.Now().UTC())
	if err != nil {
		return appctx.Principal{}, ErrUnauthenticated
	}
	client, err := s.repository.FindActiveByID(ctx, claims.OAuthClientID, s.clock.Now().UTC())
	if err != nil || client.ClientID != claims.ClientID || client.TenantID != claims.TenantID ||
		client.ApplicationID != claims.ApplicationID || client.EnvironmentID != claims.EnvironmentID ||
		client.ApplicationCode != claims.ApplicationCode || client.EnvironmentCode != claims.EnvironmentCode {
		return appctx.Principal{}, ErrUnauthenticated
	}

	scopes := make(map[string]struct{}, len(claims.Scopes))
	for _, scope := range claims.Scopes {
		if !has(client.Scopes, scope) {
			return appctx.Principal{}, ErrUnauthenticated
		}
		scopes[scope] = struct{}{}
	}
	return appctx.Principal{
		OAuthClientID:   claims.OAuthClientID,
		ClientID:        claims.ClientID,
		TenantID:        claims.TenantID,
		ApplicationID:   claims.ApplicationID,
		ApplicationCode: claims.ApplicationCode,
		EnvironmentID:   claims.EnvironmentID,
		EnvironmentCode: claims.EnvironmentCode,
		Scopes:          scopes,
	}, nil
}

func mapAuthenticationError(err error) error {
	if errors.Is(err, ErrUnauthenticated) {
		return ErrUnauthenticated
	}
	return err
}

func verifyAnySecret(credentials []domain.ClientCredential, secret string) bool {
	matched := false
	for _, credential := range credentials {
		if len(credential.SecretHash) == 0 {
			continue
		}
		if bcrypt.CompareHashAndPassword(credential.SecretHash, []byte(secret)) == nil {
			matched = true
		}
	}
	return matched
}

func allowedScopes(allowed map[string]struct{}, requested []string) ([]string, error) {
	if len(requested) == 0 {
		requested = make([]string, 0, len(allowed))
		for scope := range allowed {
			requested = append(requested, scope)
		}
	}

	seen := make(map[string]struct{}, len(requested))
	result := make([]string, 0, len(requested))
	for _, scope := range requested {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if !has(allowed, scope) {
			return nil, ErrInvalidScope
		}
		if _, exists := seen[scope]; !exists {
			seen[scope] = struct{}{}
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result, nil
}

func has(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}
