package application

import (
	"context"
	"errors"
	"net/url"
)

const loginTargetStatusActive = "ACTIVE"

// LoginTargetResolveInput identifies one pre-registered business landing target. All fields are
// required and are matched exactly; the resolver never accepts or derives a caller-supplied URI.
type LoginTargetResolveInput struct {
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
}

// LoginTarget is the minimal persistence view required by the internal runtime resolver. It is
// intentionally separate from OAuth redirect URI registrations and must never be used as an
// authorization-code callback.
type LoginTarget struct {
	ID            string
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	TargetURI     string
	Status        string
}

// LoginTargetRepository resolves an active target through the complete tenant, application and
// environment boundary. Implementations must not provide fallback or prefix matching behavior.
type LoginTargetRepository interface {
	FindActiveLoginTarget(context.Context, LoginTargetResolveInput) (LoginTarget, error)
}

// LoginTargetResolver is the internal contract used after the caller has established a trusted
// application context. It is deliberately not an anonymous HTTP-facing API.
type LoginTargetResolver interface {
	ResolveActiveTargetURI(context.Context, LoginTargetResolveInput) (string, error)
}

// LoginTargetService performs input and defensive result validation around exact persistence
// lookup. Missing, inactive or boundary-inconsistent targets are reported as not found so callers
// fail closed instead of redirecting to an unapproved address.
type LoginTargetService struct {
	repository LoginTargetRepository
}

// NewLoginTargetService constructs the internal login-target resolver.
func NewLoginTargetService(repository LoginTargetRepository) (*LoginTargetService, error) {
	if repository == nil {
		return nil, errors.New("application login target repository must not be nil")
	}
	return &LoginTargetService{repository: repository}, nil
}

// ResolveActiveTargetURI resolves one exact ACTIVE business landing target. The returned URI is
// registry-controlled data and is not an OAuth redirect_uri.
func (service *LoginTargetService) ResolveActiveTargetURI(ctx context.Context, input LoginTargetResolveInput) (string, error) {
	if !validLoginTargetResolveInput(input) {
		return "", ErrValidation
	}

	target, err := service.repository.FindActiveLoginTarget(ctx, input)
	if err != nil {
		return "", err
	}
	if target.TenantID != input.TenantID || target.ApplicationID != input.ApplicationID ||
		target.EnvironmentID != input.EnvironmentID || target.TargetCode != input.TargetCode ||
		target.Status != loginTargetStatusActive || !validLoginTargetURI(target.TargetURI) {
		return "", ErrNotFound
	}
	return target.TargetURI, nil
}

func validLoginTargetResolveInput(input LoginTargetResolveInput) bool {
	return validLoginTargetIdentifier(input.TenantID) &&
		validLoginTargetIdentifier(input.ApplicationID) &&
		validLoginTargetIdentifier(input.EnvironmentID) &&
		validCode(input.TargetCode, 64)
}

func validLoginTargetIdentifier(value string) bool {
	return len(value) == 26 && validIdentifier(value)
}

func validLoginTargetURI(value string) bool {
	if value == "" || len(value) > 2048 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil &&
		parsed.Opaque == "" && parsed.Scheme == "https"
}
