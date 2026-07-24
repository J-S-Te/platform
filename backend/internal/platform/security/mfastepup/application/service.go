// Package application coordinates authenticated MFA step-up challenges and one-time grants.
package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	mfaapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/application"
	mfadomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/domain"
)

// Repository persists the session binding and single-use lifecycle of MFA step-up grants.
type Repository interface {
	Create(context.Context, domain.Grant) error
	AuthorizeChallenge(context.Context, ChallengeAuthorization) error
	IssueGrant(context.Context, IssueGrantWrite) error
	ConsumeGrant(context.Context, ConsumeGrantWrite) error
}

// MFAChallengeService is the public MFA application capability needed for a step-up. It keeps
// TOTP secret handling and recovery-code consumption inside the identity MFA module.
type MFAChallengeService interface {
	CreateChallenge(context.Context, mfaapplication.CreateChallengeInput) (mfaapplication.CreatedChallenge, error)
	VerifyChallenge(context.Context, mfaapplication.VerifyChallengeInput) (mfaapplication.ChallengeVerification, error)
}

// IDGenerator creates durable, ULID-compatible identifiers.
type IDGenerator interface {
	New(time.Time) (string, error)
}

// Clock supports deterministic expiry tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production UTC clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ChallengeAuthorization proves that an authenticated session owns a still-pending challenge.
type ChallengeAuthorization struct {
	TenantID      string
	AccountID     string
	SessionID     string
	ChallengeHash [32]byte
	Now           time.Time
}

// IssueGrantWrite atomically transitions an MFA-verified challenge to an opaque grant.
type IssueGrantWrite struct {
	TenantID       string
	AccountID      string
	SessionID      string
	ChallengeHash  [32]byte
	GrantHash      [32]byte
	GrantedAt      time.Time
	GrantExpiresAt time.Time
}

// ConsumeGrantWrite identifies the authenticated operation that will consume a grant.
type ConsumeGrantWrite struct {
	TenantID  string
	AccountID string
	SessionID string
	GrantHash [32]byte
	Now       time.Time
}

// Config governs the lifetime of grants. The MFA module governs the challenge lifetime.
type Config struct {
	GrantTTL time.Duration
	Random   io.Reader
}

// Service implements the reusable high-risk MFA step-up boundary.
type Service struct {
	repository Repository
	mfa        MFAChallengeService
	ids        IDGenerator
	clock      Clock
	grantTTL   time.Duration
	random     io.Reader
}

// NewService validates dependencies and creates a step-up service.
func NewService(repository Repository, mfa MFAChallengeService, ids IDGenerator, clock Clock, config Config) (*Service, error) {
	if repository == nil || mfa == nil || ids == nil || clock == nil {
		return nil, errors.New("MFA step-up service dependencies must not be nil")
	}
	if config.GrantTTL <= 0 || config.GrantTTL > 15*time.Minute {
		return nil, errors.New("MFA step-up grant TTL must be between zero and fifteen minutes")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Service{repository: repository, mfa: mfa, ids: ids, clock: clock, grantTTL: config.GrantTTL, random: config.Random}, nil
}

// CreateChallengeInput is derived solely from the authenticated principal, never from request IDs.
type CreateChallengeInput struct {
	TenantID  string
	AccountID string
	SessionID string
	FactorID  string
}

// CreatedChallenge returns an opaque MFA challenge once. The plaintext must not be logged,
// audited in detail, or persisted by this module.
type CreatedChallenge struct {
	ChallengeID string
	Challenge   string
	ExpiresAt   time.Time
	MaxAttempts uint16
}

// CreateChallenge creates an identity MFA challenge and durably binds its digest to the current
// browser session before returning the opaque challenge to that same session.
func (service *Service) CreateChallenge(ctx context.Context, input CreateChallengeInput) (CreatedChallenge, error) {
	input = normalizeCreateInput(input)
	if input.TenantID == "" || input.AccountID == "" || input.SessionID == "" || input.FactorID == "" {
		return CreatedChallenge{}, domain.ErrInvalidInput
	}
	created, err := service.mfa.CreateChallenge(ctx, mfaapplication.CreateChallengeInput{TenantID: input.TenantID, AccountID: input.AccountID, FactorID: input.FactorID})
	if err != nil {
		return CreatedChallenge{}, fmt.Errorf("create MFA challenge: %w", err)
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return CreatedChallenge{}, fmt.Errorf("generate MFA step-up grant ID: %w", err)
	}
	challengeHash := sha256.Sum256([]byte(created.Challenge))
	if err := service.repository.Create(ctx, domain.Grant{
		ID: id, TenantID: input.TenantID, AccountID: input.AccountID, SessionID: input.SessionID,
		MFAChallengeID: created.ChallengeID, ChallengeHash: challengeHash, ChallengeExpiresAt: created.ExpiresAt,
		Status: domain.GrantStatusPending, CreatedAt: now,
	}); err != nil {
		return CreatedChallenge{}, fmt.Errorf("bind MFA step-up challenge to session: %w", err)
	}
	return CreatedChallenge{ChallengeID: created.ChallengeID, Challenge: created.Challenge, ExpiresAt: created.ExpiresAt, MaxAttempts: created.MaxAttempts}, nil
}

// VerifyChallengeInput carries transient secret verification material from the authenticated
// session that created the challenge. Neither Code nor Challenge is ever stored or logged here.
type VerifyChallengeInput struct {
	TenantID  string
	AccountID string
	SessionID string
	Challenge string
	Code      string
}

// VerifiedChallenge returns a one-time opaque grant after MFA verification succeeds.
type VerifiedChallenge struct {
	Grant              string
	ExpiresAt          time.Time
	VerificationMethod string
}

// VerifyChallenge ensures the challenge is owned by this session, delegates code verification to
// identity MFA, then atomically issues a short-lived grant whose digest is bound to that session.
func (service *Service) VerifyChallenge(ctx context.Context, input VerifyChallengeInput) (VerifiedChallenge, error) {
	input = normalizeVerifyInput(input)
	if input.TenantID == "" || input.AccountID == "" || input.SessionID == "" || input.Challenge == "" || len(input.Challenge) > 512 || input.Code == "" || len(input.Code) > 64 {
		return VerifiedChallenge{}, domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	challengeHash := sha256.Sum256([]byte(input.Challenge))
	if err := service.repository.AuthorizeChallenge(ctx, ChallengeAuthorization{TenantID: input.TenantID, AccountID: input.AccountID, SessionID: input.SessionID, ChallengeHash: challengeHash, Now: now}); err != nil {
		return VerifiedChallenge{}, fmt.Errorf("authorize MFA step-up challenge: %w", err)
	}
	verification, err := service.mfa.VerifyChallenge(ctx, mfaapplication.VerifyChallengeInput{Challenge: input.Challenge, Code: input.Code})
	if err != nil {
		return VerifiedChallenge{}, fmt.Errorf("verify MFA step-up challenge: %w", err)
	}
	if !verification.Verified {
		return VerifiedChallenge{}, mfadomain.ErrInvalidVerificationCode
	}
	grant, err := opaqueToken(service.random, 32)
	if err != nil {
		return VerifiedChallenge{}, fmt.Errorf("generate MFA step-up grant: %w", err)
	}
	grantHash := sha256.Sum256([]byte(grant))
	expiresAt := now.Add(service.grantTTL)
	if err := service.repository.IssueGrant(ctx, IssueGrantWrite{
		TenantID: input.TenantID, AccountID: input.AccountID, SessionID: input.SessionID,
		ChallengeHash: challengeHash, GrantHash: grantHash, GrantedAt: now, GrantExpiresAt: expiresAt,
	}); err != nil {
		return VerifiedChallenge{}, fmt.Errorf("issue MFA step-up grant: %w", err)
	}
	return VerifiedChallenge{Grant: grant, ExpiresAt: expiresAt, VerificationMethod: verification.VerificationMethod}, nil
}

// ConsumeGrant verifies the current authenticated session owns a live grant and marks it consumed
// transactionally. Call it only immediately before the high-risk handler executes.
func (service *Service) ConsumeGrant(ctx context.Context, tenantID, accountID, sessionID, grant string) error {
	tenantID, accountID, sessionID, grant = strings.TrimSpace(tenantID), strings.TrimSpace(accountID), strings.TrimSpace(sessionID), strings.TrimSpace(grant)
	if tenantID == "" || accountID == "" || sessionID == "" || grant == "" || len(grant) > 512 {
		return domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	grantHash := sha256.Sum256([]byte(grant))
	if err := service.repository.ConsumeGrant(ctx, ConsumeGrantWrite{TenantID: tenantID, AccountID: accountID, SessionID: sessionID, GrantHash: grantHash, Now: now}); err != nil {
		return fmt.Errorf("consume MFA step-up grant: %w", err)
	}
	return nil
}

func normalizeCreateInput(input CreateChallengeInput) CreateChallengeInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.FactorID = strings.TrimSpace(input.FactorID)
	return input
}

func normalizeVerifyInput(input VerifyChallengeInput) VerifyChallengeInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.Challenge = strings.TrimSpace(input.Challenge)
	input.Code = strings.TrimSpace(input.Code)
	return input
}

func opaqueToken(source io.Reader, size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := io.ReadFull(source, bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
