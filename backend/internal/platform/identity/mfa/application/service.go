// Package application coordinates TOTP enrollment and durable MFA challenge verification.
package application

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 SHA-1 TOTP is required for broad authenticator compatibility.
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/domain"
)

const (
	totpPeriodSeconds = uint64(30)
	totpDigits        = 6
)

// Repository supplies the transactional persistence operations required by MFA use cases.
type Repository interface {
	CreatePreparedFactor(context.Context, domain.TOTPFactor) error
	ConfirmPreparedFactor(context.Context, ConfirmFactorWrite, TOTPVerifier) (domain.TOTPFactor, error)
	DisableFactor(context.Context, DisableFactorWrite) (domain.TOTPFactor, error)
	CreateChallenge(context.Context, domain.MFAChallenge) error
	VerifyChallenge(context.Context, VerifyChallengeWrite, TOTPVerifier) (ChallengeVerification, error)
	CreateLoginPreAuthentication(context.Context, domain.LoginPreAuthentication) (bool, error)
	VerifyLoginPreAuthentication(context.Context, VerifyLoginPreAuthenticationWrite, TOTPVerifier) (LoginPreAuthenticationVerification, error)
}

// SecretProtector encrypts and decrypts TOTP seeds. Implementations must use authenticated
// encryption and keep key material outside the database.
type SecretProtector interface {
	Encrypt(context.Context, []byte) ([]byte, error)
	Decrypt(context.Context, []byte) ([]byte, error)
}

// IDGenerator creates ULID-compatible identifiers for factor, challenge and recovery-code rows.
type IDGenerator interface {
	New(time.Time) (string, error)
}

// Clock enables deterministic expiry and TOTP tests.
type Clock interface {
	Now() time.Time
}

// SystemClock provides the production UTC clock for MFA lifetimes and TOTP verification.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// TOTPVerifier is executed inside the repository's challenge transaction. It receives a locked
// factor snapshot so the accepted counter cannot race with another successful verification.
type TOTPVerifier func(domain.TOTPFactor, string, time.Time) (counter uint64, valid bool, err error)

// ConfirmFactorWrite is a repository command for atomically activating a prepared factor and
// creating its recovery-code digests.
type ConfirmFactorWrite struct {
	TenantID        string
	AccountID       string
	FactorID        string
	Code            string
	Now             time.Time
	ActiveExpiresAt time.Time
	RecoveryCodes   []domain.RecoveryCode
}

// DisableFactorWrite carries the optimistic-lock requirement for disabling an active factor.
type DisableFactorWrite struct {
	TenantID        string
	AccountID       string
	FactorID        string
	ExpectedVersion uint64
	Now             time.Time
}

// VerifyChallengeWrite contains no plaintext durable secrets. Code is used only during the
// transaction; CodeHash is used to locate and atomically consume a recovery-code row.
type VerifyChallengeWrite struct {
	ChallengeHash [32]byte
	CodeHash      [32]byte
	Code          string
	Now           time.Time
}

// ChallengeVerification is the transactionally final challenge state returned to the caller.
type ChallengeVerification struct {
	ChallengeID        string
	Verified           bool
	VerificationMethod string
	AttemptsRemaining  uint16
	VerifiedAt         *time.Time
	Status             string
}

// VerifyLoginPreAuthenticationWrite contains only a hash of the opaque pre-authentication
// credential. Code is retained in memory for TOTP verification and is never persisted.
type VerifyLoginPreAuthenticationWrite struct {
	CredentialHash [32]byte
	CodeHash       [32]byte
	Code           string
	Now            time.Time
}

// LoginPreAuthenticationVerification is the transactionally final pre-authentication state. The
// identity is populated only after a successful, single-use MFA verification.
type LoginPreAuthenticationVerification struct {
	Verified           bool
	VerificationMethod string
	AttemptsRemaining  uint16
	VerifiedAt         *time.Time
	Status             string
	Identity           domain.LoginPreAuthenticationIdentity
}

// Config governs MFA lifetimes and bounded verification behavior.
type Config struct {
	Issuer               string
	EnrollmentTTL        time.Duration
	FactorTTL            time.Duration
	ChallengeTTL         time.Duration
	MaxChallengeAttempts uint16
	RecoveryCodeCount    uint
	Random               io.Reader
}

// Service implements the MFA application boundary. It is intentionally independent from HTTP and
// login orchestration so an authentication flow can create a challenge after password validation.
type Service struct {
	repository        Repository
	protector         SecretProtector
	ids               IDGenerator
	clock             Clock
	issuer            string
	enrollmentTTL     time.Duration
	factorTTL         time.Duration
	challengeTTL      time.Duration
	maxAttempts       uint16
	recoveryCodeCount uint
	random            io.Reader
}

// NewService validates dependencies and constructs the MFA application service.
func NewService(repository Repository, protector SecretProtector, ids IDGenerator, clock Clock, config Config) (*Service, error) {
	if repository == nil || protector == nil || ids == nil || clock == nil {
		return nil, errors.New("MFA service dependencies must not be nil")
	}
	issuer := strings.TrimSpace(config.Issuer)
	if issuer == "" || len(issuer) > 128 {
		return nil, errors.New("MFA issuer must contain 1 to 128 characters")
	}
	if config.EnrollmentTTL <= 0 || config.FactorTTL <= 0 || config.ChallengeTTL <= 0 {
		return nil, errors.New("MFA TTL values must be greater than zero")
	}
	if config.MaxChallengeAttempts == 0 || config.MaxChallengeAttempts > 20 {
		return nil, errors.New("MFA maximum challenge attempts must be between 1 and 20")
	}
	if config.RecoveryCodeCount == 0 || config.RecoveryCodeCount > 32 {
		return nil, errors.New("MFA recovery code count must be between 1 and 32")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Service{
		repository: repository, protector: protector, ids: ids, clock: clock, issuer: issuer,
		enrollmentTTL: config.EnrollmentTTL, factorTTL: config.FactorTTL,
		challengeTTL: config.ChallengeTTL, maxAttempts: config.MaxChallengeAttempts,
		recoveryCodeCount: config.RecoveryCodeCount, random: config.Random,
	}, nil
}

// PrepareTOTPInput identifies the authenticated account enrolling a new TOTP factor.
type PrepareTOTPInput struct {
	TenantID     string
	AccountID    string
	AccountLabel string
	DisplayName  string
}

// PreparedTOTP is the one-time enrollment material. Secret must be shown only through a protected
// response and must never be logged, audited in detail or persisted in plaintext.
type PreparedTOTP struct {
	FactorID        string
	Secret          string
	ProvisioningURI string
	ExpiresAt       time.Time
}

// PrepareTOTP generates a random seed, encrypts it and persists a pending enrollment.
func (service *Service) PrepareTOTP(ctx context.Context, input PrepareTOTPInput) (PreparedTOTP, error) {
	input = normalizePrepareInput(input)
	if input.TenantID == "" || input.AccountID == "" || input.AccountLabel == "" || input.DisplayName == "" || len(input.DisplayName) > 128 {
		return PreparedTOTP{}, domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	secret, err := randomTOTPSecret(service.random)
	if err != nil {
		return PreparedTOTP{}, fmt.Errorf("generate TOTP secret: %w", err)
	}
	ciphertext, err := service.protector.Encrypt(ctx, []byte(secret))
	if err != nil {
		return PreparedTOTP{}, fmt.Errorf("encrypt TOTP secret: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext) > 1024 {
		return PreparedTOTP{}, errors.New("TOTP secret protector returned invalid ciphertext")
	}
	factorID, err := service.ids.New(now)
	if err != nil {
		return PreparedTOTP{}, fmt.Errorf("generate MFA factor ID: %w", err)
	}
	expiresAt := now.Add(service.enrollmentTTL)
	factor := domain.TOTPFactor{
		ID: factorID, TenantID: input.TenantID, AccountID: input.AccountID, DisplayName: input.DisplayName,
		SecretCiphertext: append([]byte(nil), ciphertext...), ExpiresAt: expiresAt, Status: domain.FactorStatusPending,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := service.repository.CreatePreparedFactor(ctx, factor); err != nil {
		return PreparedTOTP{}, fmt.Errorf("create prepared MFA factor: %w", err)
	}
	return PreparedTOTP{FactorID: factorID, Secret: secret, ProvisioningURI: provisioningURI(service.issuer, input.AccountLabel, secret), ExpiresAt: expiresAt}, nil
}

// ConfirmTOTPInput proves possession of a prepared TOTP seed and returns recovery codes once.
type ConfirmTOTPInput struct {
	TenantID  string
	AccountID string
	FactorID  string
	Code      string
}

// ConfirmedTOTP is returned only after a factor becomes active. RecoveryCodes are not recoverable
// from persistence; callers must display them once and direct the user to store them securely.
type ConfirmedTOTP struct {
	FactorID      string
	EnrolledAt    time.Time
	ExpiresAt     time.Time
	Version       uint64
	RecoveryCodes []string
}

// ConfirmTOTP verifies a pending seed and atomically activates the factor with hashed recovery codes.
func (service *Service) ConfirmTOTP(ctx context.Context, input ConfirmTOTPInput) (ConfirmedTOTP, error) {
	input = normalizeConfirmInput(input)
	if input.TenantID == "" || input.AccountID == "" || input.FactorID == "" || !validTOTPCode(input.Code) {
		return ConfirmedTOTP{}, domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	plainCodes, recoveryCodes, err := service.newRecoveryCodes(now, input.TenantID, input.FactorID)
	if err != nil {
		return ConfirmedTOTP{}, err
	}
	factor, err := service.repository.ConfirmPreparedFactor(ctx, ConfirmFactorWrite{
		TenantID: input.TenantID, AccountID: input.AccountID, FactorID: input.FactorID, Code: input.Code,
		Now: now, ActiveExpiresAt: now.Add(service.factorTTL), RecoveryCodes: recoveryCodes,
	}, func(factor domain.TOTPFactor, code string, at time.Time) (uint64, bool, error) {
		return service.verifyTOTP(ctx, factor, code, at)
	})
	if err != nil {
		return ConfirmedTOTP{}, fmt.Errorf("confirm MFA factor: %w", err)
	}
	return ConfirmedTOTP{FactorID: factor.ID, EnrolledAt: dereferenceTime(factor.EnrolledAt), ExpiresAt: factor.ExpiresAt, Version: factor.Version, RecoveryCodes: plainCodes}, nil
}

// DisableTOTPInput disables one factor using optimistic locking. Recovery codes remain unusable
// because every challenge verifies the factor status inside the same database transaction.
type DisableTOTPInput struct {
	TenantID        string
	AccountID       string
	FactorID        string
	ExpectedVersion uint64
}

// DisableTOTP disables the selected factor without deleting audit-relevant lifecycle state.
func (service *Service) DisableTOTP(ctx context.Context, input DisableTOTPInput) (domain.TOTPFactor, error) {
	input.TenantID, input.AccountID, input.FactorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.AccountID), strings.TrimSpace(input.FactorID)
	if input.TenantID == "" || input.AccountID == "" || input.FactorID == "" || input.ExpectedVersion == 0 {
		return domain.TOTPFactor{}, domain.ErrInvalidInput
	}
	factor, err := service.repository.DisableFactor(ctx, DisableFactorWrite{TenantID: input.TenantID, AccountID: input.AccountID, FactorID: input.FactorID, ExpectedVersion: input.ExpectedVersion, Now: service.clock.Now().UTC()})
	if err != nil {
		return domain.TOTPFactor{}, fmt.Errorf("disable MFA factor: %w", err)
	}
	return factor, nil
}

// CreateChallengeInput is supplied by the authentication flow after its primary factor succeeds.
type CreateChallengeInput struct {
	TenantID  string
	AccountID string
	FactorID  string
}

// CreatedChallenge contains the opaque token that is the sole client reference to a durable MFA
// challenge. The token is returned once; MySQL stores only its SHA-256 digest.
type CreatedChallenge struct {
	ChallengeID string
	Challenge   string
	ExpiresAt   time.Time
	MaxAttempts uint16
}

// CreateChallenge creates a bounded, single-use challenge for an active factor.
func (service *Service) CreateChallenge(ctx context.Context, input CreateChallengeInput) (CreatedChallenge, error) {
	input.TenantID, input.AccountID, input.FactorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.AccountID), strings.TrimSpace(input.FactorID)
	if input.TenantID == "" || input.AccountID == "" || input.FactorID == "" {
		return CreatedChallenge{}, domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	challenge, err := randomOpaqueToken(service.random, 32)
	if err != nil {
		return CreatedChallenge{}, fmt.Errorf("generate MFA challenge: %w", err)
	}
	challengeID, err := service.ids.New(now)
	if err != nil {
		return CreatedChallenge{}, fmt.Errorf("generate MFA challenge ID: %w", err)
	}
	hash := sha256.Sum256([]byte(challenge))
	expiresAt := now.Add(service.challengeTTL)
	if err := service.repository.CreateChallenge(ctx, domain.MFAChallenge{ID: challengeID, TenantID: input.TenantID, AccountID: input.AccountID, FactorID: input.FactorID, ChallengeHash: hash, MaxAttempts: service.maxAttempts, CreatedAt: now, ExpiresAt: expiresAt, Status: domain.ChallengeStatusPending}); err != nil {
		return CreatedChallenge{}, fmt.Errorf("create MFA challenge: %w", err)
	}
	return CreatedChallenge{ChallengeID: challengeID, Challenge: challenge, ExpiresAt: expiresAt, MaxAttempts: service.maxAttempts}, nil
}

// VerifyChallengeInput contains an opaque challenge token and either a TOTP code or recovery code.
type VerifyChallengeInput struct {
	Challenge string
	Code      string
}

// BeginLoginPreAuthenticationInput is constructed only after a local password has been verified.
type BeginLoginPreAuthenticationInput struct {
	TenantID  string
	AccountID string
}

// LoginPreAuthentication is the opaque credential returned to a client when the account has an
// active MFA factor. Credential is returned once and never stored in plaintext.
type LoginPreAuthentication struct {
	Required    bool
	Credential  string
	ExpiresAt   time.Time
	MaxAttempts uint16
}

// VerifyLoginPreAuthenticationInput contains the opaque credential and either a TOTP or recovery
// code. Tenant and account identifiers are intentionally absent from this unauthenticated input.
type VerifyLoginPreAuthenticationInput struct {
	Credential string
	Code       string
}

// VerifyChallenge verifies exactly once, enforces expiry and attempts transactionally, and consumes
// a recovery code atomically when it is used.
func (service *Service) VerifyChallenge(ctx context.Context, input VerifyChallengeInput) (ChallengeVerification, error) {
	input.Challenge, input.Code = strings.TrimSpace(input.Challenge), strings.TrimSpace(input.Code)
	if input.Challenge == "" || len(input.Challenge) > 512 || input.Code == "" || len(input.Code) > 64 {
		return ChallengeVerification{}, domain.ErrInvalidInput
	}
	canonicalRecovery := canonicalRecoveryCode(input.Code)
	challengeHash := sha256.Sum256([]byte(input.Challenge))
	codeHash := sha256.Sum256([]byte(canonicalRecovery))
	result, err := service.repository.VerifyChallenge(ctx, VerifyChallengeWrite{ChallengeHash: challengeHash, CodeHash: codeHash, Code: input.Code, Now: service.clock.Now().UTC()}, func(factor domain.TOTPFactor, code string, at time.Time) (uint64, bool, error) {
		return service.verifyTOTP(ctx, factor, code, at)
	})
	if err != nil {
		return ChallengeVerification{}, fmt.Errorf("verify MFA challenge: %w", err)
	}
	return result, nil
}

// BeginLoginPreAuthentication creates a short-lived, hashed and account-bound credential only
// when an active MFA factor exists. Accounts without active MFA factors preserve password-only
// login behavior and do not receive a pre-authentication credential.
func (service *Service) BeginLoginPreAuthentication(ctx context.Context, input BeginLoginPreAuthenticationInput) (LoginPreAuthentication, error) {
	input.TenantID, input.AccountID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.AccountID)
	if input.TenantID == "" || input.AccountID == "" {
		return LoginPreAuthentication{}, domain.ErrInvalidInput
	}
	now := service.clock.Now().UTC()
	credential, err := randomOpaqueToken(service.random, 32)
	if err != nil {
		return LoginPreAuthentication{}, fmt.Errorf("generate MFA login pre-authentication credential: %w", err)
	}
	id, err := service.ids.New(now)
	if err != nil {
		return LoginPreAuthentication{}, fmt.Errorf("generate MFA login pre-authentication ID: %w", err)
	}
	hash := sha256.Sum256([]byte(credential))
	expiresAt := now.Add(service.challengeTTL)
	required, err := service.repository.CreateLoginPreAuthentication(ctx, domain.LoginPreAuthentication{
		ID: id, TenantID: input.TenantID, AccountID: input.AccountID, CredentialHash: hash,
		MaxAttempts: service.maxAttempts, CreatedAt: now, ExpiresAt: expiresAt,
		Status: domain.LoginPreAuthenticationStatusPending,
	})
	if err != nil {
		return LoginPreAuthentication{}, fmt.Errorf("create MFA login pre-authentication: %w", err)
	}
	if !required {
		return LoginPreAuthentication{}, nil
	}
	return LoginPreAuthentication{Required: true, Credential: credential, ExpiresAt: expiresAt, MaxAttempts: service.maxAttempts}, nil
}

// VerifyLoginPreAuthentication verifies a TOTP or recovery code, consumes the matching
// pre-authentication record exactly once and returns only server-trusted identity attributes.
func (service *Service) VerifyLoginPreAuthentication(ctx context.Context, input VerifyLoginPreAuthenticationInput) (LoginPreAuthenticationVerification, error) {
	input.Credential, input.Code = strings.TrimSpace(input.Credential), strings.TrimSpace(input.Code)
	if input.Credential == "" || len(input.Credential) > 512 || input.Code == "" || len(input.Code) > 64 {
		return LoginPreAuthenticationVerification{}, domain.ErrInvalidInput
	}
	canonicalRecovery := canonicalRecoveryCode(input.Code)
	credentialHash := sha256.Sum256([]byte(input.Credential))
	codeHash := sha256.Sum256([]byte(canonicalRecovery))
	result, err := service.repository.VerifyLoginPreAuthentication(ctx, VerifyLoginPreAuthenticationWrite{
		CredentialHash: credentialHash, CodeHash: codeHash, Code: input.Code, Now: service.clock.Now().UTC(),
	}, func(factor domain.TOTPFactor, code string, at time.Time) (uint64, bool, error) {
		return service.verifyTOTP(ctx, factor, code, at)
	})
	if err != nil {
		return LoginPreAuthenticationVerification{}, fmt.Errorf("verify MFA login pre-authentication: %w", err)
	}
	return result, nil
}

func (service *Service) verifyTOTP(ctx context.Context, factor domain.TOTPFactor, code string, now time.Time) (uint64, bool, error) {
	plaintext, err := service.protector.Decrypt(ctx, factor.SecretCiphertext)
	if err != nil {
		return 0, false, fmt.Errorf("decrypt TOTP secret: %w", err)
	}
	defer zero(plaintext)
	return verifyTOTP(string(plaintext), code, now, factor.LastAcceptedCounter)
}

func (service *Service) newRecoveryCodes(now time.Time, tenantID, factorID string) ([]string, []domain.RecoveryCode, error) {
	plaintext := make([]string, 0, service.recoveryCodeCount)
	records := make([]domain.RecoveryCode, 0, service.recoveryCodeCount)
	seen := make(map[string]struct{}, service.recoveryCodeCount)
	for len(plaintext) < int(service.recoveryCodeCount) {
		code, err := randomRecoveryCode(service.random)
		if err != nil {
			return nil, nil, fmt.Errorf("generate MFA recovery code: %w", err)
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		id, err := service.ids.New(now)
		if err != nil {
			return nil, nil, fmt.Errorf("generate MFA recovery code ID: %w", err)
		}
		hash := sha256.Sum256([]byte(code))
		plaintext = append(plaintext, code)
		records = append(records, domain.RecoveryCode{ID: id, TenantID: tenantID, FactorID: factorID, CodeHash: hash, CreatedAt: now})
	}
	return plaintext, records, nil
}

func normalizePrepareInput(input PrepareTOTPInput) PrepareTOTPInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.AccountLabel = strings.TrimSpace(input.AccountLabel)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	return input
}

func normalizeConfirmInput(input ConfirmTOTPInput) ConfirmTOTPInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.FactorID = strings.TrimSpace(input.FactorID)
	input.Code = strings.TrimSpace(input.Code)
	return input
}

func randomTOTPSecret(source io.Reader) (string, error) {
	bytes := make([]byte, 20)
	if _, err := io.ReadFull(source, bytes); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes), nil
}

func randomOpaqueToken(source io.Reader, size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := io.ReadFull(source, bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func randomRecoveryCode(source io.Reader) (string, error) {
	bytes := make([]byte, 10)
	if _, err := io.ReadFull(source, bytes); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes), nil
}

func provisioningURI(issuer, accountLabel, secret string) string {
	label := url.PathEscape(issuer + ":" + accountLabel)
	values := url.Values{}
	values.Set("secret", secret)
	values.Set("issuer", issuer)
	values.Set("algorithm", "SHA1")
	values.Set("digits", strconv.Itoa(totpDigits))
	values.Set("period", strconv.FormatUint(totpPeriodSeconds, 10))
	return "otpauth://totp/" + label + "?" + values.Encode()
}

func verifyTOTP(secret, code string, now time.Time, lastAccepted *uint64) (uint64, bool, error) {
	if !validTOTPCode(code) {
		return 0, false, nil
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, false, errors.New("invalid decrypted TOTP secret")
	}
	defer zero(key)
	current := uint64(now.UTC().Unix()) / totpPeriodSeconds
	for offset := int64(-1); offset <= 1; offset++ {
		if offset < 0 && current < uint64(-offset) {
			continue
		}
		counter := uint64(int64(current) + offset)
		if lastAccepted != nil && counter <= *lastAccepted {
			continue
		}
		expected := totpCode(key, counter)
		if hmac.Equal([]byte(expected), []byte(code)) {
			return counter, true, nil
		}
	}
	return 0, false, nil
}

func totpCode(key []byte, counter uint64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	mac := hmac.New(sha1.New, key) // #nosec G401 -- RFC 6238 SHA-1 interoperability profile.
	_, _ = mac.Write(message[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 0x0f)
	value := (uint32(digest[offset]&0x7f)<<24 | uint32(digest[offset+1])<<16 | uint32(digest[offset+2])<<8 | uint32(digest[offset+3])) % 1_000_000
	return fmt.Sprintf("%06d", value)
}

func validTOTPCode(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func canonicalRecoveryCode(code string) string {
	var builder strings.Builder
	for _, character := range strings.ToUpper(strings.TrimSpace(code)) {
		if character == '-' || unicode.IsSpace(character) {
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

func dereferenceTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func zero(bytes []byte) {
	for index := range bytes {
		bytes[index] = 0
	}
}
