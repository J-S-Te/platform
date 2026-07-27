package application

import (
	"context"
	"errors"
	"net/url"
	"strings"
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
	FindActiveEnvironment(context.Context, string, string, string) (Environment, error)
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
//
// TargetURI accepts two forms:
//   - Absolute https URL: returned as-is.
//   - Absolute path beginning with a single '/': resolved against the parent environment's public
//     BaseURL and optional PathPrefix so administrators can register sub-systems without
//     hard-coding the portal host and port. A relative TargetURI without a usable BaseURL is
//     treated as a resolution failure and reported as not found.
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
	if isRelativeLoginTargetURI(target.TargetURI) {
		environment, err := service.repository.FindActiveEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
		if err != nil || environment.BaseURL == nil || *environment.BaseURL == "" {
			return "", ErrNotFound
		}
		return joinEnvironmentBaseURLAndTargetURI(*environment.BaseURL, environment.PathPrefix, target.TargetURI)
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

// validLoginTargetURI accepts both an absolute public URL and an absolute path under the portal.
//
//   - Absolute public URL: scheme https, host non-empty, no userinfo, no opaque component.
//   - Relative path: a single leading '/', no scheme, no query/fragment, ASCII printable only.
//
// Pure hostname strings (e.g. "example.com"), scheme-relative ("//x") and protocol-bearing
// garbage are rejected to keep this from being abused as an open redirect helper.
func validLoginTargetURI(value string) bool {
	if value == "" || len(value) > 2048 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	if isRelativeLoginTargetURI(value) {
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil &&
		parsed.Opaque == "" && strings.EqualFold(parsed.Scheme, "https")
}

// isRelativeLoginTargetURI returns true when value is a safe absolute path the resolver is
// allowed to expand against the parent environment's BaseURL.
func isRelativeLoginTargetURI(value string) bool {
	if !validPortalPath(value, 2048) {
		return false
	}
	return true
}

// joinBaseURLAndTargetURI stitches a parent BaseURL and a portal-relative TargetURI into one
// externally visible landing URL. Trailing slashes on the base and leading slashes on the target
// are normalized; the path part of the base is preserved (so a BaseURL of http://h/api/v1 still
// keeps /api/v1 in front of the target).
func joinBaseURLAndTargetURI(baseURL, targetURI string) (string, error) {
	return joinEnvironmentBaseURLAndTargetURI(baseURL, nil, targetURI)
}

// joinEnvironmentBaseURLAndTargetURI composes the externally visible landing URL from three
// independently managed values: the portal BaseURL, the environment PathPrefix and the target's
// relative path. Internal UpstreamURL values never participate in browser redirects.
func joinEnvironmentBaseURLAndTargetURI(baseURL string, pathPrefix *string, targetURI string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrNotFound
	}
	if !isRelativeLoginTargetURI(targetURI) {
		return "", ErrNotFound
	}

	prefix := ""
	if pathPrefix != nil && *pathPrefix != "" {
		prefix = strings.TrimRight(*pathPrefix, "/")
		if !validPortalPath(prefix, 128) {
			return "", ErrNotFound
		}
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + prefix + targetURI
	parsed.RawPath = ""
	return parsed.String(), nil
}
