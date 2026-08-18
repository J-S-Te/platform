package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// BootstrapTenantCode is the migration-seeded tenant used by the controlled initial setup.
	BootstrapTenantCode = "default"
	// BootstrapApplicationCode is the migration-seeded platform application.
	BootstrapApplicationCode = "platform"
	// BootstrapSuperAdminRoleCode is the built-in role assigned only by this initial setup.
	BootstrapSuperAdminRoleCode = "platform-super-admin"
	// BootstrapRootOrganizationCode is the migration-seeded organization that owns the first
	// platform administrator's primary appointment.
	BootstrapRootOrganizationCode = "ROOT"
	// BootstrapSuperAdminPositionCode is the migration-seeded position used by the first
	// platform administrator. Keeping this a code rather than a display name makes the
	// bootstrap contract independent from localized position labels.
	BootstrapSuperAdminPositionCode = "POS-01J00000000000000000000400"
)

var (
	// ErrBootstrapAlreadyInitialized means the controlled first-administrator flow was completed.
	ErrBootstrapAlreadyInitialized = errors.New("first super administrator is already initialized")
	// ErrBootstrapUnavailable means the required migration-seeded platform records are unavailable.
	ErrBootstrapUnavailable = errors.New("first super administrator bootstrap is unavailable")
)

// PasswordHasher creates a password digest and its verification metadata without exposing either
// implementation details or plaintext passwords to the identity application service.
type PasswordHasher interface {
	Hash(password string) (digest []byte, metadata []byte, err error)
}

// BootstrapRepository atomically creates the first local account, its primary organization
// membership, and its platform super-admin role binding. The implementation must serialize
// requests for the default tenant.
type BootstrapRepository interface {
	BootstrapFirstSuperAdmin(context.Context, BootstrapWrite) (BootstrapResult, error)
}

// BootstrapInput contains the only operator-supplied values accepted by the one-time flow.
type BootstrapInput struct {
	DisplayName string
	AccountName string
	Password    string
}

// BootstrapWrite is the validated, secret-safe persistence command. PasswordDigest and
// AlgorithmParams contain no plaintext password.
type BootstrapWrite struct {
	BootstrapID     string
	UserID          string
	AccountID       string
	CredentialID    string
	MembershipID    string
	RoleBindingID   string
	DisplayName     string
	AccountName     string
	PasswordDigest  []byte
	AlgorithmParams []byte
	InitializedAt   time.Time
}

// BootstrapResult is safe to return to the HTTP adapter. It intentionally excludes password,
// password hash, algorithm metadata, and bootstrap token values.
type BootstrapResult struct {
	BootstrapID   string
	TenantID      string
	TenantCode    string
	UserID        string
	DisplayName   string
	AccountID     string
	AccountName   string
	RoleID        string
	RoleCode      string
	RoleName      string
	InitializedAt time.Time
}

// BootstrapService coordinates the controlled, one-time first-super-administrator initialization.
type BootstrapService struct {
	repository BootstrapRepository
	hasher     PasswordHasher
	ids        IDGenerator
	clock      Clock
}

// NewBootstrapService constructs the first-super-administrator use case.
func NewBootstrapService(repository BootstrapRepository, hasher PasswordHasher, ids IDGenerator, clock Clock) (*BootstrapService, error) {
	if repository == nil || hasher == nil || ids == nil || clock == nil {
		return nil, errors.New("identity bootstrap service dependencies must not be nil")
	}
	return &BootstrapService{repository: repository, hasher: hasher, ids: ids, clock: clock}, nil
}

// InitializeFirstSuperAdmin validates input, hashes the operator password once, and delegates the
// all-or-nothing state transition to the GORM repository. It never logs or returns the password.
func (service *BootstrapService) InitializeFirstSuperAdmin(ctx context.Context, input BootstrapInput) (BootstrapResult, error) {
	if err := validateBootstrapInput(input); err != nil {
		return BootstrapResult{}, err
	}

	digest, metadata, err := service.hasher.Hash(input.Password)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("hash first super administrator password: %w", err)
	}
	now := service.clock.Now().UTC()
	bootstrapID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap state ID: %w", err)
	}
	userID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap user ID: %w", err)
	}
	accountID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap account ID: %w", err)
	}
	credentialID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap credential ID: %w", err)
	}
	membershipID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap membership ID: %w", err)
	}
	roleBindingID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap role binding ID: %w", err)
	}

	return service.repository.BootstrapFirstSuperAdmin(ctx, BootstrapWrite{
		BootstrapID: bootstrapID, UserID: userID, AccountID: accountID, CredentialID: credentialID,
		MembershipID: membershipID, RoleBindingID: roleBindingID, DisplayName: strings.TrimSpace(input.DisplayName),
		AccountName: strings.TrimSpace(input.AccountName), PasswordDigest: digest,
		AlgorithmParams: metadata, InitializedAt: now,
	})
}

func validateBootstrapInput(input BootstrapInput) error {
	if !validName(input.DisplayName, 100) {
		return ErrValidation
	}
	if !validAccountName(input.AccountName) || validateStrongPassword(input.Password) != nil {
		return ErrValidation
	}
	return nil
}
