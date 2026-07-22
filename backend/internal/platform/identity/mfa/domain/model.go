// Package domain contains the MFA aggregate and its stable lifecycle states.
package domain

import (
	"errors"
	"time"
)

const (
	// FactorStatusPending means a TOTP seed was issued but its enrollment has not been confirmed.
	FactorStatusPending = "PENDING"
	// FactorStatusActive means the factor can be used to verify MFA challenges.
	FactorStatusActive = "ACTIVE"
	// FactorStatusDisabled means the factor must never be used again.
	FactorStatusDisabled = "DISABLED"

	// ChallengeStatusPending means a challenge may still be verified.
	ChallengeStatusPending = "PENDING"
	// ChallengeStatusVerified means the challenge was verified exactly once.
	ChallengeStatusVerified = "VERIFIED"
	// ChallengeStatusExpired means the challenge reached its expiry before successful verification.
	ChallengeStatusExpired = "EXPIRED"
	// ChallengeStatusRejected means verification attempts were exhausted or its factor became unavailable.
	ChallengeStatusRejected = "REJECTED"

	// VerificationMethodTOTP identifies a successful TOTP verification.
	VerificationMethodTOTP = "TOTP"
	// VerificationMethodRecoveryCode identifies a successful one-time recovery-code verification.
	VerificationMethodRecoveryCode = "RECOVERY_CODE"
)

var (
	ErrInvalidInput              = errors.New("invalid MFA input")
	ErrFactorNotFound            = errors.New("MFA factor not found")
	ErrFactorUnavailable         = errors.New("MFA factor is unavailable")
	ErrEnrollmentExpired         = errors.New("MFA enrollment expired")
	ErrInvalidVerificationCode   = errors.New("invalid MFA verification code")
	ErrVersionConflict           = errors.New("MFA version conflict")
	ErrChallengeNotFound         = errors.New("MFA challenge not found")
	ErrChallengeExpired          = errors.New("MFA challenge expired")
	ErrChallengeConsumed         = errors.New("MFA challenge already consumed")
	ErrChallengeAttemptsExceeded = errors.New("MFA challenge attempts exceeded")
)

// TOTPFactor is the durable MFA aggregate. SecretCiphertext is encrypted by an application-owned
// protector; the plaintext seed is never represented by a persisted domain field.
type TOTPFactor struct {
	ID                  string
	TenantID            string
	AccountID           string
	DisplayName         string
	SecretCiphertext    []byte
	EnrolledAt          *time.Time
	LastUsedAt          *time.Time
	LastAcceptedCounter *uint64
	DisabledAt          *time.Time
	ExpiresAt           time.Time
	Status              string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Version             uint64
}

// MFAChallenge is a durable, single-use verification challenge. ChallengeHash is SHA-256 of the
// opaque client token and prevents the token itself from entering MySQL.
type MFAChallenge struct {
	ID            string
	TenantID      string
	AccountID     string
	FactorID      string
	ChallengeHash [32]byte
	AttemptCount  uint16
	MaxAttempts   uint16
	CreatedAt     time.Time
	ExpiresAt     time.Time
	VerifiedAt    *time.Time
	Status        string
}

// RecoveryCode contains only a SHA-256 digest. Plaintext recovery codes are returned once during
// enrollment confirmation and must not be logged or persisted.
type RecoveryCode struct {
	ID         string
	TenantID   string
	FactorID   string
	CodeHash   [32]byte
	CreatedAt  time.Time
	ConsumedAt *time.Time
}

// IsActiveAt reports whether a factor can accept a new MFA challenge at the supplied instant.
func (factor TOTPFactor) IsActiveAt(at time.Time) bool {
	return factor.Status == FactorStatusActive && factor.DisabledAt == nil && at.Before(factor.ExpiresAt)
}

// IsPendingAt reports whether an enrollment may still be confirmed.
func (factor TOTPFactor) IsPendingAt(at time.Time) bool {
	return factor.Status == FactorStatusPending && factor.EnrolledAt == nil && at.Before(factor.ExpiresAt)
}

const (
	// LoginPreAuthenticationStatusPending means the password was verified and MFA still must be
	// completed before a browser session may be issued.
	LoginPreAuthenticationStatusPending = "PENDING"
	// LoginPreAuthenticationStatusConsumed means the record was used to complete one MFA login.
	LoginPreAuthenticationStatusConsumed = "CONSUMED"
	// LoginPreAuthenticationStatusExpired means the record reached its short expiry window.
	LoginPreAuthenticationStatusExpired = "EXPIRED"
	// LoginPreAuthenticationStatusRejected means its bounded MFA verification attempts were used.
	LoginPreAuthenticationStatusRejected = "REJECTED"
)

var (
	ErrLoginPreAuthenticationNotFound         = errors.New("MFA login pre-authentication not found")
	ErrLoginPreAuthenticationExpired          = errors.New("MFA login pre-authentication expired")
	ErrLoginPreAuthenticationConsumed         = errors.New("MFA login pre-authentication already consumed")
	ErrLoginPreAuthenticationAttemptsExceeded = errors.New("MFA login pre-authentication attempts exceeded")
)

// LoginPreAuthentication contains the durable state between a verified password and verified MFA.
// CredentialHash is SHA-256 of an opaque client credential; the credential itself is never stored.
type LoginPreAuthentication struct {
	ID             string
	TenantID       string
	AccountID      string
	CredentialHash [32]byte
	AttemptCount   uint16
	MaxAttempts    uint16
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
	Status         string
}

// LoginPreAuthenticationIdentity is the server-trusted identity reconstructed after MFA succeeds.
// It is never accepted from the unauthenticated MFA verification request.
type LoginPreAuthenticationIdentity struct {
	TenantID    string
	TenantName  string
	TenantCode  string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}
