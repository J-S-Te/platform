package application

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

const (
	accountNameMinLength = 3
	accountNameMaxLength = 64
	passwordMinRunes     = 8
	passwordMaxRunes     = 128
	generatedPasswordLen = 24
)

var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// PasswordGenerator creates a one-time strong password for an administrator to deliver offline.
type PasswordGenerator interface {
	Generate() (string, error)
}

// CryptoPasswordGenerator uses crypto/rand and always produces a value accepted by
// validateStrongPassword. Generated passwords are never persisted or logged by this package.
type CryptoPasswordGenerator struct{}

// Generate creates a 24-character password with upper/lowercase letters, digits and symbols.
func (CryptoPasswordGenerator) Generate() (string, error) {
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const lower = "abcdefghijkmnopqrstuvwxyz"
	const digits = "23456789"
	const symbols = "!@#$%^&*-_=+"
	const all = upper + lower + digits + symbols

	sets := []string{upper, lower, digits, symbols}
	password := make([]byte, 0, generatedPasswordLen)
	for _, set := range sets {
		character, err := randomCharacter(set)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for len(password) < generatedPasswordLen {
		character, err := randomCharacter(all)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for index := len(password) - 1; index > 0; index-- {
		randomIndex, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return "", fmt.Errorf("randomize generated password: %w", err)
		}
		selected := int(randomIndex.Int64())
		password[index], password[selected] = password[selected], password[index]
	}
	return string(password), nil
}

func randomCharacter(characters string) (byte, error) {
	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(characters))))
	if err != nil {
		return 0, fmt.Errorf("select random password character: %w", err)
	}
	return characters[index.Int64()], nil
}

// LocalPasswordCredential contains the minimum secret-bearing data required to verify a current
// password. It must remain inside the application and infrastructure layers.
type LocalPasswordCredential struct {
	TenantID         string
	AccountID        string
	AccountStatus    string
	CredentialStatus string
	PasswordHash     []byte
	HashAlgorithm    string
	AlgorithmParams  []byte
}

// LocalAccountCreateInput is the public use-case input for a normal local human account.
type LocalAccountCreateInput struct {
	TenantID        string
	OperatorID      string
	UserID          string
	AccountName     string
	InitialPassword string
	ValidUntil      *time.Time
}

// PasswordInitializeInput initializes a missing local password credential exactly once.
type PasswordInitializeInput struct {
	TenantID   string
	OperatorID string
	AccountID  string
	Password   string
	Version    uint64
}

// PasswordResetInput requests a server-generated password that is returned only to the
// administrator's current HTTP response for offline delivery.
type PasswordResetInput struct {
	TenantID   string
	OperatorID string
	AccountID  string
	Version    uint64
}

// PasswordChangeInput changes the authenticated account's own local password.
type PasswordChangeInput struct {
	TenantID        string
	AccountID       string
	OperatorID      string
	CurrentPassword string
	NewPassword     string
}

// LocalAccountCreateWrite is a secret-safe persistence command. It deliberately holds only a
// password digest and its verification metadata, never a plaintext password.
type LocalAccountCreateWrite struct {
	AccountID       string
	CredentialID    string
	TenantID        string
	UserID          string
	OperatorID      string
	AccountName     string
	PasswordDigest  []byte
	AlgorithmParams []byte
	OccurredAt      time.Time
	ValidUntil      *time.Time
}

// PasswordWrite updates a local credential and revokes active browser sessions atomically.
type PasswordWrite struct {
	CredentialID    string
	TenantID        string
	AccountID       string
	OperatorID      string
	ExpectedVersion uint64
	ExpectedHash    []byte
	PasswordDigest  []byte
	AlgorithmParams []byte
	OccurredAt      time.Time
	RevokeReason    string
}

// AccountLifecycleRepository owns the transaction boundaries for local-account and credential
// mutations. Implementations must not store or log plaintext passwords.
type AccountLifecycleRepository interface {
	CreateLocalAccount(context.Context, LocalAccountCreateWrite) (domain.Account, error)
	InitializePassword(context.Context, PasswordWrite) (domain.Account, error)
	ResetPassword(context.Context, PasswordWrite) (domain.Account, error)
	FindLocalPasswordCredential(context.Context, string, string) (LocalPasswordCredential, error)
	ChangeOwnPassword(context.Context, PasswordWrite) error
}

// PasswordResetResult contains the one-time plaintext generated for an administrator. Callers
// must render it only in the immediate response and never persist, log or audit it.
type PasswordResetResult struct {
	AccountID         string
	TemporaryPassword string
}

// AccountLifecycleService implements normal local account provisioning and password lifecycle.
type AccountLifecycleService struct {
	repository        AccountLifecycleRepository
	hasher            PasswordHasher
	verifier          PasswordVerifier
	passwordGenerator PasswordGenerator
	ids               IDGenerator
	clock             Clock
}

// NewAccountLifecycleService constructs the account lifecycle service.
func NewAccountLifecycleService(repository AccountLifecycleRepository, hasher PasswordHasher, verifier PasswordVerifier, passwordGenerator PasswordGenerator, ids IDGenerator, clock Clock) (*AccountLifecycleService, error) {
	if repository == nil || hasher == nil || verifier == nil || passwordGenerator == nil || ids == nil || clock == nil {
		return nil, errors.New("account lifecycle service dependencies must not be nil")
	}
	return &AccountLifecycleService{repository: repository, hasher: hasher, verifier: verifier, passwordGenerator: passwordGenerator, ids: ids, clock: clock}, nil
}

// CreateLocalAccount creates an active HUMAN/LOCAL account and an Argon2id credential atomically.
func (service *AccountLifecycleService) CreateLocalAccount(ctx context.Context, input LocalAccountCreateInput) (domain.Account, error) {
	if err := validateLocalAccountCreateInput(input); err != nil {
		return domain.Account{}, err
	}
	digest, metadata, err := service.hasher.Hash(input.InitialPassword)
	if err != nil {
		return domain.Account{}, fmt.Errorf("hash initial password: %w", err)
	}
	now := service.clock.Now().UTC()
	if input.ValidUntil != nil && !input.ValidUntil.After(now) {
		return domain.Account{}, ErrValidation
	}
	accountID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate account ID: %w", err)
	}
	credentialID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate password credential ID: %w", err)
	}
	return service.repository.CreateLocalAccount(ctx, LocalAccountCreateWrite{
		AccountID: accountID, CredentialID: credentialID, TenantID: strings.TrimSpace(input.TenantID), UserID: strings.TrimSpace(input.UserID), OperatorID: strings.TrimSpace(input.OperatorID), AccountName: strings.TrimSpace(input.AccountName), PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: now, ValidUntil: normalizedFutureTime(input.ValidUntil),
	})
}

// InitializePassword sets a strong local password only when no credential exists for the account.
func (service *AccountLifecycleService) InitializePassword(ctx context.Context, input PasswordInitializeInput) (domain.Account, error) {
	if err := validatePasswordInitializeInput(input); err != nil {
		return domain.Account{}, err
	}
	now := service.clock.Now().UTC()
	credentialID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate password credential ID: %w", err)
	}
	return service.writeAdministratorPassword(ctx, input.TenantID, input.AccountID, input.OperatorID, input.Version, input.Password, credentialID, "PASSWORD_INITIALIZED")
}

// ResetPassword creates a strong password, returns it once, replaces the stored digest and revokes
// every active session of the target account. The plaintext never crosses the repository boundary.
func (service *AccountLifecycleService) ResetPassword(ctx context.Context, input PasswordResetInput) (PasswordResetResult, error) {
	if err := validatePasswordResetInput(input); err != nil {
		return PasswordResetResult{}, err
	}
	password, err := service.passwordGenerator.Generate()
	if err != nil {
		return PasswordResetResult{}, fmt.Errorf("generate reset password: %w", err)
	}
	if err := validateStrongPassword(password); err != nil {
		return PasswordResetResult{}, fmt.Errorf("generated password violates policy: %w", err)
	}
	if _, err = service.writeAdministratorPassword(ctx, input.TenantID, input.AccountID, input.OperatorID, input.Version, password, "", "ADMIN_PASSWORD_RESET"); err != nil {
		return PasswordResetResult{}, err
	}
	return PasswordResetResult{AccountID: strings.TrimSpace(input.AccountID), TemporaryPassword: password}, nil
}

// ChangeOwnPassword verifies the current password, replaces its digest and revokes every session,
// including the current browser session. The caller must clear its session cookie afterwards.
func (service *AccountLifecycleService) ChangeOwnPassword(ctx context.Context, input PasswordChangeInput) error {
	if err := validatePasswordChangeInput(input); err != nil {
		return err
	}
	credential, err := service.repository.FindLocalPasswordCredential(ctx, strings.TrimSpace(input.TenantID), strings.TrimSpace(input.AccountID))
	if err != nil {
		return err
	}
	if credential.AccountStatus != domain.StatusActive || credential.CredentialStatus != domain.StatusActive {
		return ErrConflict
	}
	matched, err := service.verifier.Verify(input.CurrentPassword, credential.HashAlgorithm, credential.PasswordHash, credential.AlgorithmParams)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !matched {
		return ErrUnauthenticated
	}
	digest, metadata, err := service.hasher.Hash(input.NewPassword)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	return service.repository.ChangeOwnPassword(ctx, PasswordWrite{
		TenantID: strings.TrimSpace(input.TenantID), AccountID: strings.TrimSpace(input.AccountID), OperatorID: strings.TrimSpace(input.OperatorID), ExpectedHash: append([]byte(nil), credential.PasswordHash...), PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: service.clock.Now().UTC(), RevokeReason: "SELF_PASSWORD_CHANGED",
	})
}

func (service *AccountLifecycleService) writeAdministratorPassword(ctx context.Context, tenantID, accountID, operatorID string, version uint64, password, credentialID, reason string) (domain.Account, error) {
	digest, metadata, err := service.hasher.Hash(password)
	if err != nil {
		return domain.Account{}, fmt.Errorf("hash password: %w", err)
	}
	write := PasswordWrite{CredentialID: credentialID, TenantID: strings.TrimSpace(tenantID), AccountID: strings.TrimSpace(accountID), OperatorID: strings.TrimSpace(operatorID), ExpectedVersion: version, PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: service.clock.Now().UTC(), RevokeReason: reason}
	if reason == "PASSWORD_INITIALIZED" {
		return service.repository.InitializePassword(ctx, write)
	}
	return service.repository.ResetPassword(ctx, write)
}

func validateLocalAccountCreateInput(input LocalAccountCreateInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.UserID) == "" || !validAccountName(input.AccountName) || validateStrongPassword(input.InitialPassword) != nil {
		return ErrValidation
	}
	return nil
}

func normalizedFutureTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func validatePasswordInitializeInput(input PasswordInitializeInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.AccountID) == "" || input.Version == 0 || validateStrongPassword(input.Password) != nil {
		return ErrValidation
	}
	return nil
}

func validatePasswordResetInput(input PasswordResetInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.AccountID) == "" || input.Version == 0 {
		return ErrValidation
	}
	return nil
}

func validatePasswordChangeInput(input PasswordChangeInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.OperatorID) == "" || input.CurrentPassword == "" || validateStrongPassword(input.NewPassword) != nil {
		return ErrValidation
	}
	return nil
}

func validAccountName(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= accountNameMinLength && len(value) <= accountNameMaxLength && accountNamePattern.MatchString(value)
}

// validateStrongPassword enforces the shared account-password policy: 8–128 runes, no Unicode
// whitespace, and at least one uppercase letter, lowercase letter, digit and symbol.
func validateStrongPassword(password string) error {
	length := len([]rune(password))
	if length < passwordMinRunes || length > passwordMaxRunes {
		return ErrValidation
	}
	var upper, lower, digit, symbol bool
	for _, character := range password {
		if unicode.IsSpace(character) {
			return ErrValidation
		}
		switch {
		case unicode.IsUpper(character):
			upper = true
		case unicode.IsLower(character):
			lower = true
		case unicode.IsDigit(character):
			digit = true
		default:
			symbol = true
		}
	}
	if !upper || !lower || !digit || !symbol {
		return ErrValidation
	}
	return nil
}
