// Package domain defines the durable state and invariant errors for high-risk MFA step-up grants.
package domain

import (
	"errors"
	"time"
)

const (
	// GrantStatusPending identifies a session-bound MFA challenge awaiting verification.
	GrantStatusPending = "PENDING"
	// GrantStatusIssued identifies a short-lived grant that has not yet authorized an operation.
	GrantStatusIssued = "ISSUED"
	// GrantStatusConsumed identifies a grant that has authorized exactly one operation.
	GrantStatusConsumed = "CONSUMED"
	// GrantStatusExpired identifies a challenge or grant that reached its expiry.
	GrantStatusExpired = "EXPIRED"
)

var (
	ErrInvalidInput        = errors.New("invalid MFA step-up input")
	ErrChallengeNotFound   = errors.New("MFA step-up challenge not found")
	ErrChallengeExpired    = errors.New("MFA step-up challenge expired")
	ErrChallengeBinding    = errors.New("MFA step-up challenge does not belong to this session")
	ErrChallengeNotPending = errors.New("MFA step-up challenge is no longer pending")
	ErrGrantNotFound       = errors.New("MFA step-up grant not found")
	ErrGrantExpired        = errors.New("MFA step-up grant expired")
	ErrGrantConsumed       = errors.New("MFA step-up grant already consumed")
	ErrGrantBinding        = errors.New("MFA step-up grant does not belong to this session")
	ErrGrantNotIssued      = errors.New("MFA step-up grant is unavailable")
)

// Grant is the durable state for a session-bound MFA step-up verification. The database stores
// only SHA-256 digests of the opaque challenge and grant values; neither plaintext is persisted.
type Grant struct {
	ID                 string
	TenantID           string
	AccountID          string
	SessionID          string
	MFAChallengeID     string
	ChallengeHash      [32]byte
	GrantHash          *[32]byte
	ChallengeExpiresAt time.Time
	GrantExpiresAt     *time.Time
	GrantedAt          *time.Time
	ConsumedAt         *time.Time
	Status             string
	CreatedAt          time.Time
}
