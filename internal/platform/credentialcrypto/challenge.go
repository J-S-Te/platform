package credentialcrypto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	PurposeLogin           = "LOGIN"
	PurposeChangePassword  = "CHANGE_PASSWORD"
	PurposeCreateAccount   = "CREATE_ACCOUNT"
	PurposeCreateEmployee  = "CREATE_EMPLOYEE"
	PurposeCreateEmployees = "CREATE_EMPLOYEES_BATCH"
	PurposeInitialize      = "INITIALIZE_PASSWORD"
	PurposeReset           = "RESET_PASSWORD"
	DefaultChallengeTTL    = 60 * time.Second
	MinChallengeTTL        = 30 * time.Second
	MaxChallengeTTL        = 120 * time.Second
	MaxCleanupBatch        = 1000
)

var ValidPurposes = []string{
	PurposeLogin, PurposeChangePassword, PurposeCreateAccount, PurposeCreateEmployee,
	PurposeCreateEmployees, PurposeInitialize, PurposeReset,
}

var ErrChallengeInvalid = errors.New("CREDENTIAL_CHALLENGE_INVALID")
var ErrChallengeExpired = errors.New("CREDENTIAL_CHALLENGE_EXPIRED")
var ErrChallengeUsed = errors.New("CREDENTIAL_CHALLENGE_USED")

type Purpose struct {
	Code string
	// ResourceBound is true only for operations addressing an existing account.
	ResourceBound bool
	// Anonymous is true only for the login pre-authentication challenge.
	Anonymous bool
	RouteKey  string
}

var Purposes = map[string]Purpose{
	PurposeLogin:           {Code: PurposeLogin, Anonymous: true, RouteKey: "auth.login"},
	PurposeChangePassword:  {Code: PurposeChangePassword, RouteKey: "auth.password.change"},
	PurposeCreateAccount:   {Code: PurposeCreateAccount, RouteKey: "account.create"},
	PurposeCreateEmployee:  {Code: PurposeCreateEmployee, RouteKey: "employee.create"},
	PurposeCreateEmployees: {Code: PurposeCreateEmployees, RouteKey: "employee.batch"},
	PurposeInitialize:      {Code: PurposeInitialize, ResourceBound: true, RouteKey: "account.password.initialize"},
	PurposeReset:           {Code: PurposeReset, ResourceBound: true, RouteKey: "account.password.reset"},
}

type Challenge struct {
	IDHash      []byte
	KID         string
	Purpose     string
	ResourceID  string
	TenantID    string
	BindingHash []byte
	HTTPMethod  string
	RouteKey    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type ChallengeRepository interface {
	Create(context.Context, Challenge) error
	Consume(context.Context, Challenge) error
	DeleteExpired(context.Context, time.Time, int) (int64, error)
}

type ChallengeService struct {
	Repository       ChallengeRepository
	Now              func() time.Time
	TTL              time.Duration
	CleanupBatchSize int
}

type IssuedChallenge struct {
	Raw        string
	KID        string
	Purpose    string
	ResourceID string
	ExpiresAt  time.Time
}

func HashBinding(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func hashID(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func NewRawID() (string, error) {
	raw, err := randomBytes(32)
	if err != nil {
		return "", ErrEncryptionFailed
	}
	return hex.EncodeToString(raw), nil
}

func normalizePurpose(code string) (Purpose, bool) {
	code = strings.TrimSpace(code)
	purpose, ok := Purposes[code]
	return purpose, ok
}

func (service *ChallengeService) ttl() time.Duration {
	if service.TTL >= MinChallengeTTL && service.TTL <= MaxChallengeTTL {
		return service.TTL
	}
	return DefaultChallengeTTL
}

func (service *ChallengeService) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}

// Issue creates a one-time challenge. raw is returned only to the client; the
// database stores only its SHA-256 digest.
func (service *ChallengeService) Issue(ctx context.Context, kid, purposeCode, resourceID, tenantID string, bindingHash []byte) (IssuedChallenge, error) {
	purpose, ok := normalizePurpose(purposeCode)
	if !ok || kid == "" || len(bindingHash) != sha256.Size {
		return IssuedChallenge{}, ErrChallengeInvalid
	}
	if purpose.ResourceBound != (resourceID != "") {
		return IssuedChallenge{}, ErrChallengeInvalid
	}
	raw, err := NewRawID()
	if err != nil {
		return IssuedChallenge{}, err
	}
	now := service.now()
	expires := now.Add(service.ttl())
	record := Challenge{
		IDHash: hashID(raw), KID: kid, Purpose: purpose.Code, ResourceID: resourceID,
		TenantID: tenantID, BindingHash: append([]byte(nil), bindingHash...),
		HTTPMethod: "POST", RouteKey: purpose.RouteKey, CreatedAt: now, ExpiresAt: expires,
	}
	if err := service.Repository.Create(ctx, record); err != nil {
		return IssuedChallenge{}, err
	}
	return IssuedChallenge{Raw: raw, KID: kid, Purpose: purpose.Code, ResourceID: resourceID, ExpiresAt: expires}, nil
}

// Consume atomically marks the challenge used. A business failure must not make
// the same challenge reusable; callers commit this before executing business work.
func (service *ChallengeService) Consume(ctx context.Context, raw, kid, purposeCode, resourceID, tenantID string, bindingHash []byte) error {
	if raw == "" || kid == "" || len(bindingHash) != sha256.Size {
		return ErrChallengeInvalid
	}
	purpose, ok := normalizePurpose(purposeCode)
	if !ok || purpose.ResourceBound != (resourceID != "") {
		return ErrChallengeInvalid
	}
	record := Challenge{
		IDHash: hashID(raw), KID: kid, Purpose: purpose.Code, ResourceID: resourceID,
		TenantID: tenantID, BindingHash: append([]byte(nil), bindingHash...),
		HTTPMethod: "POST", RouteKey: purpose.RouteKey,
	}
	err := service.Repository.Consume(ctx, record)
	if errors.Is(err, ErrChallengeUsed) || errors.Is(err, ErrChallengeExpired) {
		return err
	}
	return err
}

func (service *ChallengeService) Cleanup(ctx context.Context) (int64, error) {
	cutoff := service.now().Add(-5 * time.Minute)
	limit := service.CleanupBatchSize
	if limit <= 0 || limit > MaxCleanupBatch {
		limit = MaxCleanupBatch
	}
	return service.Repository.DeleteExpired(ctx, cutoff, limit)
}

// StartCleanup runs a bounded cleanup loop. It is intentionally best-effort:
// cleanup failure never bypasses challenge consumption or expires a challenge early.
func (service *ChallengeService) StartCleanup(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := service.Cleanup(ctx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
