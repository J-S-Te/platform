// Package accesssubject binds compact OIDC JWT claims to active tenant-scoped storage records.
package accesssubject

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
)

// repository is the minimal lookup set needed by UserInfo. Both lookups independently require
// active client/session state; their tenant and subject identifiers are then cross-checked here.
type repository interface {
	FindClient(context.Context, string, time.Time) (domain.Client, error)
	ResolveSessionSubject(context.Context, string, time.Time) (domain.SessionSubject, error)
}

// Clock supplies the authoritative validation time for the active-session lookup.
type Clock interface {
	Now() time.Time
}

// Resolver is an OIDC HTTP AccessTokenSubjectResolver backed by the OIDC repository.
type Resolver struct {
	repository repository
	clock      Clock
}

// New creates a resolver. The passed repository may be the production GORM OIDC repository.
func New(repository repository, clock Clock) (*Resolver, error) {
	if repository == nil || clock == nil {
		return nil, errors.New("OIDC access-token subject resolver dependencies must not be nil")
	}
	return &Resolver{repository: repository, clock: clock}, nil
}

// ResolveAccessTokenSubject verifies that client_id, session ID, and user subject from the signed
// access token still refer to one active tenant-scoped client/session combination.
func (resolver *Resolver) ResolveAccessTokenSubject(ctx context.Context, clientID, sessionID, userID string) (oidchttp.AccessTokenSubject, error) {
	if resolver == nil || resolver.repository == nil || resolver.clock == nil {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject resolver is not initialized")
	}
	clientID, sessionID, userID = strings.TrimSpace(clientID), strings.TrimSpace(sessionID), strings.TrimSpace(userID)
	if clientID == "" || sessionID == "" || userID == "" {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject is incomplete")
	}
	now := resolver.clock.Now().UTC()
	client, err := resolver.repository.FindClient(ctx, clientID, now)
	if err != nil {
		return oidchttp.AccessTokenSubject{}, err
	}
	subject, err := resolver.repository.ResolveSessionSubject(ctx, sessionID, now)
	if err != nil {
		return oidchttp.AccessTokenSubject{}, err
	}
	if subject.TenantID != client.TenantID || subject.UserID != userID {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject does not match active storage")
	}
	return oidchttp.AccessTokenSubject{TenantID: subject.TenantID, OAuthClientID: client.ID}, nil
}

var _ oidchttp.AccessTokenSubjectResolver = (*Resolver)(nil)
