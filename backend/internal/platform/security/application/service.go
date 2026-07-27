// Package application coordinates login-policy and account-lockout use cases.
package application

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/domain"
)

var (
	// ErrNotFound hides tenant-scoped resources that are absent or not visible to the caller.
	ErrNotFound = errors.New("security resource not found")
	// ErrConflict reports an invalid state transition or optimistic-lock conflict.
	ErrConflict = errors.New("security resource conflict")
	// ErrValidation reports unsafe or incomplete input.
	ErrValidation = errors.New("security validation failed")
)

const (
	defaultMaxFailedAttempts         = 5
	defaultLockoutDurationSeconds    = 15 * 60
	defaultFailureResetWindowSeconds = 30 * 60
	defaultIdleTimeoutSeconds        = 30 * 60
)

// Clock enables deterministic security-policy tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production UTC clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// LoginFailureInput contains the trusted account and client data for one failed password attempt.
// Passwords are deliberately absent from this contract.
type LoginFailureInput struct {
	TenantID    string
	AccountID   string
	AccountName string
	IPAddress   net.IP
	UserAgent   string
}

// LoginFailureRecorder allows the identity module to persist a failed attempt without depending
// on security storage details.
type LoginFailureRecorder interface {
	RecordFailedLogin(context.Context, LoginFailureInput) (LoginFailureResult, error)
}

// SessionIdleTimeoutProvider exposes the tenant-specific inactivity timeout without leaking
// security storage details into the identity module.
type SessionIdleTimeoutProvider interface {
	SessionIdleTimeout(context.Context, string) (time.Duration, error)
}

// LoginSecurityService is the dependency required by identity authentication.
type LoginSecurityService interface {
	LoginFailureRecorder
	SessionIdleTimeoutProvider
}

// LoginFailureResult tells the authentication flow whether this failed attempt created a lock.
type LoginFailureResult struct {
	LockedUntil *time.Time
}

// LoginPolicyUpdateInput is the replace payload for one tenant login policy.
type LoginPolicyUpdateInput struct {
	TenantID                  string
	OperatorID                string
	MaxFailedAttempts         uint
	LockoutDurationSeconds    uint
	FailureResetWindowSeconds uint
	IdleTimeoutSeconds        uint
	Version                   uint64
}

// PageRequest contains list filters for locked-account queries.
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
}

// PageResult is a tenant-scoped, page-number based result set.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// UnlockInput identifies an administrator-initiated account unlock.
type UnlockInput struct {
	TenantID   string
	AccountID  string
	OperatorID string
}

// Repository persists login-security aggregates. All operations must enforce tenant scope.
type Repository interface {
	GetLoginPolicy(context.Context, string) (domain.LoginPolicy, error)
	UpdateLoginPolicy(context.Context, LoginPolicyUpdateInput, time.Time) (domain.LoginPolicy, error)
	RecordFailedLogin(context.Context, LoginFailureInput, domain.LoginPolicy, time.Time) (LoginFailureResult, error)
	ListLockedAccounts(context.Context, string, PageRequest, time.Time) (PageResult[domain.LockedAccount], error)
	UnlockAccount(context.Context, UnlockInput, time.Time) (domain.LockedAccount, error)
}

// Service applies login-security validation before repository operations.
type Service struct {
	repository Repository
	clock      Clock
}

// NewService creates a security application service.
func NewService(repository Repository, clock Clock) (*Service, error) {
	if repository == nil || clock == nil {
		return nil, errors.New("security service dependencies must not be nil")
	}
	return &Service{repository: repository, clock: clock}, nil
}

// GetLoginPolicy returns a persisted policy, or documented defaults for a tenant that has not
// explicitly customized its policy yet.
func (service *Service) GetLoginPolicy(ctx context.Context, tenantID string) (domain.LoginPolicy, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.LoginPolicy{}, ErrValidation
	}
	policy, err := service.repository.GetLoginPolicy(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return defaultLoginPolicy(tenantID), nil
	}
	return policy, err
}

// UpdateLoginPolicy validates and persists an administrator-selected tenant login policy.
func (service *Service) UpdateLoginPolicy(ctx context.Context, input LoginPolicyUpdateInput) (domain.LoginPolicy, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" ||
		!validPolicy(input.MaxFailedAttempts, input.LockoutDurationSeconds, input.FailureResetWindowSeconds, input.IdleTimeoutSeconds) {
		return domain.LoginPolicy{}, ErrValidation
	}
	return service.repository.UpdateLoginPolicy(ctx, input, service.clock.Now().UTC())
}

// SessionIdleTimeout returns the effective inactivity timeout for a tenant. A session that has
// no authenticated activity for this duration must be revoked by the identity repository.
func (service *Service) SessionIdleTimeout(ctx context.Context, tenantID string) (time.Duration, error) {
	policy, err := service.GetLoginPolicy(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return time.Duration(policy.IdleTimeoutSeconds) * time.Second, nil
}

// RecordFailedLogin applies the current tenant policy to one verified-account password failure.
func (service *Service) RecordFailedLogin(ctx context.Context, input LoginFailureInput) (LoginFailureResult, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.AccountName) == "" {
		return LoginFailureResult{}, ErrValidation
	}
	policy, err := service.GetLoginPolicy(ctx, input.TenantID)
	if err != nil {
		return LoginFailureResult{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	return service.repository.RecordFailedLogin(ctx, input, policy, now)
}

// ListLockedAccounts returns currently locked accounts visible to a tenant security administrator.
func (service *Service) ListLockedAccounts(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.LockedAccount], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.LockedAccount]{}, ErrValidation
	}
	return service.repository.ListLockedAccounts(ctx, tenantID, normalizePage(query), service.clock.Now().UTC())
}

// UnlockAccount clears the account lock and logically closes outstanding failed-login attempts.
func (service *Service) UnlockAccount(ctx context.Context, input UnlockInput) (domain.LockedAccount, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.OperatorID) == "" {
		return domain.LockedAccount{}, ErrValidation
	}
	return service.repository.UnlockAccount(ctx, input, service.clock.Now().UTC())
}

func defaultLoginPolicy(tenantID string) domain.LoginPolicy {
	return domain.LoginPolicy{
		TenantID: tenantID, MaxFailedAttempts: defaultMaxFailedAttempts,
		LockoutDurationSeconds:    defaultLockoutDurationSeconds,
		FailureResetWindowSeconds: defaultFailureResetWindowSeconds,
		IdleTimeoutSeconds:        defaultIdleTimeoutSeconds,
		Version:                   1,
	}
}

func validPolicy(maxAttempts, lockoutSeconds, resetSeconds, idleTimeoutSeconds uint) bool {
	return maxAttempts >= 1 && maxAttempts <= 20 &&
		lockoutSeconds >= 60 && lockoutSeconds <= 24*60*60 &&
		resetSeconds >= 60 && resetSeconds <= 24*60*60 &&
		idleTimeoutSeconds >= 60 && idleTimeoutSeconds <= 24*60*60
}

func normalizePage(query PageRequest) PageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	return query
}
