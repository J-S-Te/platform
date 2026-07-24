package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	loginTargetStatusDisabled  = "DISABLED"
	defaultLoginTargetFallback = "/"
)

// LoginTargetManagementItem is the tenant-scoped control-plane representation of one approved
// post-login landing target. It deliberately does not include any OAuth redirect URI data.
type LoginTargetManagementItem struct {
	ID            string
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	Name          string
	TargetURI     string
	Status        string
	Version       uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// LoginTargetCreateInput defines the only mutable fields accepted when an administrator registers
// a new approved post-login target. The application and environment boundary is supplied by the
// protected route and must be verified by the repository before the row is inserted.
type LoginTargetCreateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	Name          string
	TargetURI     string
	Status        string
}

// LoginTargetUpdateInput modifies a registered target with optimistic locking. TargetCode is
// immutable because callers may depend on it as a stable login request contract.
type LoginTargetUpdateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	LoginTargetID string
	Name          string
	TargetURI     string
	Status        string
	Version       uint64
}

// LoginTargetManagementRepository persists management operations and verifies the complete
// tenant/application/environment boundary. It must never infer an environment from a URI.
type LoginTargetManagementRepository interface {
	EnsureLoginTargetBoundary(context.Context, string, string, string) error
	ListLoginTargets(context.Context, string, string, string, PageRequest) (PageResult[LoginTargetManagementItem], error)
	CreateLoginTarget(context.Context, LoginTargetCreateInput, string, time.Time) (LoginTargetManagementItem, error)
	GetLoginTarget(context.Context, string, string, string, string) (LoginTargetManagementItem, error)
	UpdateLoginTarget(context.Context, LoginTargetUpdateInput, time.Time) (LoginTargetManagementItem, error)
}

// LoginTargetManagementService coordinates the control plane for approved cross-application
// landing targets. Runtime resolution remains a separate read-only service so these targets can
// never be mistaken for OAuth authorization-code callback registrations.
type LoginTargetManagementService struct {
	repository LoginTargetManagementRepository
	ids        IdentifierGenerator
	clock      Clock
}

// NewLoginTargetManagementService constructs login-target management use cases.
func NewLoginTargetManagementService(repository LoginTargetManagementRepository, ids IdentifierGenerator, clock Clock) (*LoginTargetManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("application login target management dependencies must not be nil")
	}
	return &LoginTargetManagementService{repository: repository, ids: ids, clock: clock}, nil
}

// ListLoginTargets lists targets only after the requested parent boundary has been verified. This
// prevents a valid tenant from probing arbitrary application or environment identifiers.
func (service *LoginTargetManagementService) ListLoginTargets(ctx context.Context, tenantID, applicationID, environmentID string, query PageRequest) (PageResult[LoginTargetManagementItem], error) {
	tenantID, applicationID, environmentID = normalizeLoginTargetBoundary(tenantID, applicationID, environmentID)
	query = normalizePageRequest(query)
	if !validLoginTargetBoundary(tenantID, applicationID, environmentID) || !validLoginTargetStatusFilter(query.Status) {
		return PageResult[LoginTargetManagementItem]{}, ErrValidation
	}
	if err := service.repository.EnsureLoginTargetBoundary(ctx, tenantID, applicationID, environmentID); err != nil {
		return PageResult[LoginTargetManagementItem]{}, err
	}
	return service.repository.ListLoginTargets(ctx, tenantID, applicationID, environmentID, query)
}

// CreateLoginTarget registers one exact, administrator-approved external landing URI.
func (service *LoginTargetManagementService) CreateLoginTarget(ctx context.Context, input LoginTargetCreateInput) (LoginTargetManagementItem, error) {
	input = normalizeLoginTargetCreate(input)
	if !validLoginTargetCreate(input) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	if err := service.repository.EnsureLoginTargetBoundary(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID); err != nil {
		return LoginTargetManagementItem{}, err
	}

	now := service.clock.Now().UTC()
	identifier, err := service.ids.New(now)
	if err != nil {
		return LoginTargetManagementItem{}, fmt.Errorf("generate application login target ID: %w", err)
	}
	return service.repository.CreateLoginTarget(ctx, input, identifier, now)
}

// GetLoginTarget returns one target within the exact tenant/application/environment boundary.
func (service *LoginTargetManagementService) GetLoginTarget(ctx context.Context, tenantID, applicationID, environmentID, loginTargetID string) (LoginTargetManagementItem, error) {
	tenantID, applicationID, environmentID = normalizeLoginTargetBoundary(tenantID, applicationID, environmentID)
	loginTargetID = strings.TrimSpace(loginTargetID)
	if !validLoginTargetBoundary(tenantID, applicationID, environmentID) || !validLoginTargetIdentifier(loginTargetID) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	return service.repository.GetLoginTarget(ctx, tenantID, applicationID, environmentID, loginTargetID)
}

// UpdateLoginTarget performs a versioned update without changing its stable target code.
func (service *LoginTargetManagementService) UpdateLoginTarget(ctx context.Context, input LoginTargetUpdateInput) (LoginTargetManagementItem, error) {
	input = normalizeLoginTargetUpdate(input)
	if !validLoginTargetUpdate(input) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	return service.repository.UpdateLoginTarget(ctx, input, service.clock.Now().UTC())
}

// LoginTargetRedirectDecision gives the shared login flow a safe redirect result. If no target was
// requested, the existing platform-local fallback is retained. If an approved target is requested
// but cannot be resolved, callers must fail the login transition rather than redirect to a
// caller-supplied or environment-default URI.
type LoginTargetRedirectDecision struct {
	Location       string
	TargetResolved bool
}

// ResolvePostLoginRedirect resolves a requested target only from registry-controlled data. The
// fallback is accepted only as a platform-local relative path; external, protocol-relative and
// malformed values collapse to "/". A target-resolution failure is returned to the caller so it
// can stop the login completion and present a stable platform error page.
func (service *LoginTargetService) ResolvePostLoginRedirect(ctx context.Context, input *LoginTargetResolveInput, platformFallback string) (LoginTargetRedirectDecision, error) {
	fallback := SafePlatformLoginFallback(platformFallback)
	if input == nil {
		return LoginTargetRedirectDecision{Location: fallback}, nil
	}

	location, err := service.ResolveActiveTargetURI(ctx, *input)
	if err != nil {
		return LoginTargetRedirectDecision{Location: fallback}, err
	}
	return LoginTargetRedirectDecision{Location: location, TargetResolved: true}, nil
}

// SafePlatformLoginFallback restricts an exceptional post-login fallback to this platform. It is
// intentionally not an open redirect helper and must not be used to validate application targets.
func SafePlatformLoginFallback(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return defaultLoginTargetFallback
	}
	if !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") || strings.Contains(candidate, "\\") {
		return defaultLoginTargetFallback
	}
	for _, character := range []byte(candidate) {
		if character < 0x21 || character > 0x7e {
			return defaultLoginTargetFallback
		}
	}
	return candidate
}

func normalizeLoginTargetBoundary(tenantID, applicationID, environmentID string) (string, string, string) {
	return strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID)
}

func normalizeLoginTargetCreate(input LoginTargetCreateInput) LoginTargetCreateInput {
	input.TenantID, input.ApplicationID, input.EnvironmentID = normalizeLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.TargetCode = strings.TrimSpace(input.TargetCode)
	input.Name = strings.TrimSpace(input.Name)
	input.TargetURI = strings.TrimSpace(input.TargetURI)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeLoginTargetUpdate(input LoginTargetUpdateInput) LoginTargetUpdateInput {
	input.TenantID, input.ApplicationID, input.EnvironmentID = normalizeLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.LoginTargetID = strings.TrimSpace(input.LoginTargetID)
	input.Name = strings.TrimSpace(input.Name)
	input.TargetURI = strings.TrimSpace(input.TargetURI)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func validLoginTargetBoundary(tenantID, applicationID, environmentID string) bool {
	return validLoginTargetIdentifier(tenantID) && validLoginTargetIdentifier(applicationID) && validLoginTargetIdentifier(environmentID)
}

func validLoginTargetCreate(input LoginTargetCreateInput) bool {
	return validLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID) && validLoginTargetIdentifier(input.OperatorID) &&
		validCode(input.TargetCode, 64) && validManagementText(input.Name, 128, false) &&
		validLoginTargetURI(input.TargetURI) && validLoginTargetStatus(input.Status)
}

func validLoginTargetUpdate(input LoginTargetUpdateInput) bool {
	return validLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID) && validLoginTargetIdentifier(input.OperatorID) &&
		validLoginTargetIdentifier(input.LoginTargetID) && input.Version > 0 && validManagementText(input.Name, 128, false) &&
		validLoginTargetURI(input.TargetURI) && validLoginTargetStatus(input.Status)
}

func validLoginTargetStatus(value string) bool {
	return value == loginTargetStatusActive || value == loginTargetStatusDisabled
}

func validLoginTargetStatusFilter(value string) bool {
	return value == "" || validLoginTargetStatus(value)
}
