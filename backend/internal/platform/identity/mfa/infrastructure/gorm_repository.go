// Package infrastructure provides the GORM persistence adapter for the MFA tables created by
// migration 000020_create_identity_enhancements.sql.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository persists TOTP factors, opaque challenges and recovery-code digests with explicit
// transactions. It never calls AutoMigrate.
type Repository struct {
	database *gorm.DB
}

// NewRepository constructs an MFA repository using the shared GORM database connection.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("MFA GORM database must not be nil")
	}
	return &Repository{database: database}, nil
}

type totpFactorModel struct {
	ID                  string     `gorm:"column:id"`
	TenantID            string     `gorm:"column:tenant_id"`
	AccountID           string     `gorm:"column:account_id"`
	DisplayName         string     `gorm:"column:display_name"`
	SecretCiphertext    []byte     `gorm:"column:secret_ciphertext"`
	EnrolledAt          *time.Time `gorm:"column:enrolled_at"`
	LastUsedAt          *time.Time `gorm:"column:last_used_at"`
	LastAcceptedCounter *uint64    `gorm:"column:last_accepted_counter"`
	DisabledAt          *time.Time `gorm:"column:disabled_at"`
	ExpiresAt           time.Time  `gorm:"column:expires_at"`
	Status              string     `gorm:"column:status"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
	Version             uint64     `gorm:"column:version"`
}

func (totpFactorModel) TableName() string { return "iam_mfa_totp_factor" }

type challengeModel struct {
	ID            string     `gorm:"column:id"`
	TenantID      string     `gorm:"column:tenant_id"`
	AccountID     string     `gorm:"column:account_id"`
	FactorID      string     `gorm:"column:factor_id"`
	ChallengeHash []byte     `gorm:"column:challenge_hash"`
	AttemptCount  uint16     `gorm:"column:attempt_count"`
	MaxAttempts   uint16     `gorm:"column:max_attempts"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	ExpiresAt     time.Time  `gorm:"column:expires_at"`
	VerifiedAt    *time.Time `gorm:"column:verified_at"`
	Status        string     `gorm:"column:status"`
}

func (challengeModel) TableName() string { return "iam_mfa_challenge" }

type recoveryCodeModel struct {
	ID         string     `gorm:"column:id"`
	TenantID   string     `gorm:"column:tenant_id"`
	FactorID   string     `gorm:"column:factor_id"`
	CodeHash   []byte     `gorm:"column:code_hash"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
}

func (recoveryCodeModel) TableName() string { return "iam_mfa_recovery_code" }

// CreatePreparedFactor persists a pending, encrypted TOTP enrollment.
func (repository *Repository) CreatePreparedFactor(ctx context.Context, factor domain.TOTPFactor) error {
	row := toFactorModel(factor)
	return repository.database.WithContext(ctx).Create(&row).Error
}

// ConfirmPreparedFactor locks the pending factor, verifies the code against its encrypted seed and
// atomically transitions it to ACTIVE while inserting only recovery-code digests.
func (repository *Repository) ConfirmPreparedFactor(ctx context.Context, write application.ConfirmFactorWrite, verifier application.TOTPVerifier) (domain.TOTPFactor, error) {
	var confirmed domain.TOTPFactor
	var terminalErr error
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row totpFactorModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND account_id = ? AND id = ?", write.TenantID, write.AccountID, write.FactorID).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			terminalErr = domain.ErrFactorNotFound
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock prepared MFA factor: %w", result.Error)
		}
		factor := toFactor(row)
		if factor.Status != domain.FactorStatusPending {
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}
		if !factor.IsPendingAt(write.Now) {
			if err := transaction.Model(&totpFactorModel{}).Where("id = ? AND status = ?", factor.ID, domain.FactorStatusPending).Updates(map[string]any{"status": domain.FactorStatusDisabled, "disabled_at": write.Now, "updated_at": write.Now, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return fmt.Errorf("expire MFA enrollment: %w", err)
			}
			terminalErr = domain.ErrEnrollmentExpired
			return nil
		}
		counter, valid, err := verifier(factor, write.Code, write.Now)
		if err != nil {
			return fmt.Errorf("verify prepared TOTP factor: %w", err)
		}
		if !valid {
			terminalErr = domain.ErrInvalidVerificationCode
			return nil
		}
		enrolledAt := write.Now
		update := transaction.Model(&totpFactorModel{}).Where("id = ? AND status = ? AND version = ?", factor.ID, domain.FactorStatusPending, factor.Version).Updates(map[string]any{
			"enrolled_at": enrolledAt, "last_used_at": enrolledAt, "last_accepted_counter": counter, "expires_at": write.ActiveExpiresAt,
			"status": domain.FactorStatusActive, "updated_at": write.Now, "version": gorm.Expr("version + 1"),
		})
		if update.Error != nil {
			return fmt.Errorf("activate MFA factor: %w", update.Error)
		}
		if update.RowsAffected != 1 {
			terminalErr = domain.ErrVersionConflict
			return nil
		}
		for _, code := range write.RecoveryCodes {
			row := toRecoveryCodeModel(code)
			if err := transaction.Create(&row).Error; err != nil {
				return fmt.Errorf("create MFA recovery-code digest: %w", err)
			}
		}
		row.EnrolledAt, row.LastUsedAt = &enrolledAt, &enrolledAt
		row.LastAcceptedCounter, row.ExpiresAt, row.Status, row.UpdatedAt, row.Version = &counter, write.ActiveExpiresAt, domain.FactorStatusActive, write.Now, row.Version+1
		confirmed = toFactor(row)
		return nil
	})
	if err != nil {
		return domain.TOTPFactor{}, err
	}
	if terminalErr != nil {
		return domain.TOTPFactor{}, terminalErr
	}
	return confirmed, nil
}

// DisableFactor transitions an active factor to DISABLED using an explicit optimistic-lock version.
func (repository *Repository) DisableFactor(ctx context.Context, write application.DisableFactorWrite) (domain.TOTPFactor, error) {
	var disabled domain.TOTPFactor
	var terminalErr error
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row totpFactorModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND account_id = ? AND id = ?", write.TenantID, write.AccountID, write.FactorID).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			terminalErr = domain.ErrFactorNotFound
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA factor for disabling: %w", result.Error)
		}
		if row.Version != write.ExpectedVersion {
			terminalErr = domain.ErrVersionConflict
			return nil
		}
		if row.Status != domain.FactorStatusActive || row.DisabledAt != nil {
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}
		disabledAt := write.Now
		update := transaction.Model(&totpFactorModel{}).Where("id = ? AND version = ? AND status = ?", row.ID, write.ExpectedVersion, domain.FactorStatusActive).Updates(map[string]any{"status": domain.FactorStatusDisabled, "disabled_at": disabledAt, "updated_at": write.Now, "version": gorm.Expr("version + 1")})
		if update.Error != nil {
			return fmt.Errorf("disable MFA factor: %w", update.Error)
		}
		if update.RowsAffected != 1 {
			terminalErr = domain.ErrVersionConflict
			return nil
		}
		row.Status, row.DisabledAt, row.UpdatedAt, row.Version = domain.FactorStatusDisabled, &disabledAt, write.Now, row.Version+1
		disabled = toFactor(row)
		return nil
	})
	if err != nil {
		return domain.TOTPFactor{}, err
	}
	if terminalErr != nil {
		return domain.TOTPFactor{}, terminalErr
	}
	return disabled, nil
}

// CreateChallenge verifies the target factor remains active before creating a durable challenge.
func (repository *Repository) CreateChallenge(ctx context.Context, challenge domain.MFAChallenge) error {
	var terminalErr error
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var factor totpFactorModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND account_id = ? AND id = ?", challenge.TenantID, challenge.AccountID, challenge.FactorID).First(&factor)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			terminalErr = domain.ErrFactorNotFound
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA factor for challenge: %w", result.Error)
		}
		if !toFactor(factor).IsActiveAt(challenge.CreatedAt) {
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}
		row := toChallengeModel(challenge)
		if err := transaction.Create(&row).Error; err != nil {
			return fmt.Errorf("create MFA challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return terminalErr
}

// VerifyChallenge locks the challenge, factor and (when applicable) recovery code in one
// transaction. Thus expiry, retry limits, TOTP counter advancement and recovery-code consumption
// cannot race between concurrent requests.
func (repository *Repository) VerifyChallenge(ctx context.Context, write application.VerifyChallengeWrite, verifier application.TOTPVerifier) (application.ChallengeVerification, error) {
	var verification application.ChallengeVerification
	var terminalErr error
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var challenge challengeModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("challenge_hash = ?", write.ChallengeHash[:]).First(&challenge)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			terminalErr = domain.ErrChallengeNotFound
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA challenge: %w", result.Error)
		}
		verification = application.ChallengeVerification{ChallengeID: challenge.ID, Status: challenge.Status}
		if challenge.Status == domain.ChallengeStatusVerified {
			terminalErr = domain.ErrChallengeConsumed
			return nil
		}
		if challenge.Status == domain.ChallengeStatusExpired || !write.Now.Before(challenge.ExpiresAt) {
			if challenge.Status == domain.ChallengeStatusPending {
				if err := transaction.Model(&challengeModel{}).Where("id = ? AND status = ?", challenge.ID, domain.ChallengeStatusPending).Updates(map[string]any{"status": domain.ChallengeStatusExpired}).Error; err != nil {
					return fmt.Errorf("expire MFA challenge: %w", err)
				}
			}
			verification.Status = domain.ChallengeStatusExpired
			terminalErr = domain.ErrChallengeExpired
			return nil
		}
		if challenge.Status != domain.ChallengeStatusPending {
			terminalErr = domain.ErrChallengeAttemptsExceeded
			return nil
		}

		var factor totpFactorModel
		result = transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND account_id = ? AND id = ?", challenge.TenantID, challenge.AccountID, challenge.FactorID).First(&factor)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			if err := rejectUnavailableFactor(transaction, &challenge, &verification); err != nil {
				return err
			}
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA challenge factor: %w", result.Error)
		}
		factorDomain := toFactor(factor)
		if !factorDomain.IsActiveAt(write.Now) {
			if err := rejectUnavailableFactor(transaction, &challenge, &verification); err != nil {
				return err
			}
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}

		method, counter, verified, err := verifyCodeInTransaction(transaction, factorDomain, write, verifier)
		if err != nil {
			return err
		}
		if !verified {
			return recordFailedAttempt(transaction, &challenge, &verification)
		}
		if method == domain.VerificationMethodTOTP {
			usedAt := write.Now
			update := transaction.Model(&totpFactorModel{}).Where("id = ? AND status = ? AND (last_accepted_counter IS NULL OR last_accepted_counter < ?)", factor.ID, domain.FactorStatusActive, counter).Updates(map[string]any{"last_accepted_counter": counter, "last_used_at": usedAt, "updated_at": usedAt, "version": gorm.Expr("version + 1")})
			if update.Error != nil {
				return fmt.Errorf("advance MFA TOTP counter: %w", update.Error)
			}
			if update.RowsAffected != 1 {
				return recordFailedAttempt(transaction, &challenge, &verification)
			}
		}
		verifiedAt := write.Now
		update := transaction.Model(&challengeModel{}).Where("id = ? AND status = ?", challenge.ID, domain.ChallengeStatusPending).Updates(map[string]any{"status": domain.ChallengeStatusVerified, "verified_at": verifiedAt})
		if update.Error != nil {
			return fmt.Errorf("consume MFA challenge: %w", update.Error)
		}
		if update.RowsAffected != 1 {
			terminalErr = domain.ErrChallengeConsumed
			return nil
		}
		verification = application.ChallengeVerification{ChallengeID: challenge.ID, Verified: true, VerificationMethod: method, AttemptsRemaining: challenge.MaxAttempts - challenge.AttemptCount, VerifiedAt: &verifiedAt, Status: domain.ChallengeStatusVerified}
		return nil
	})
	if err != nil {
		return application.ChallengeVerification{}, err
	}
	if terminalErr != nil {
		return verification, terminalErr
	}
	return verification, nil
}

func verifyCodeInTransaction(transaction *gorm.DB, factor domain.TOTPFactor, write application.VerifyChallengeWrite, verifier application.TOTPVerifier) (string, uint64, bool, error) {
	var recovery recoveryCodeModel
	result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("factor_id = ? AND code_hash = ? AND consumed_at IS NULL", factor.ID, write.CodeHash[:]).First(&recovery)
	if result.Error == nil {
		consumedAt := write.Now
		update := transaction.Model(&recoveryCodeModel{}).Where("id = ? AND consumed_at IS NULL", recovery.ID).Updates(map[string]any{"consumed_at": consumedAt})
		if update.Error != nil {
			return "", 0, false, fmt.Errorf("consume MFA recovery code: %w", update.Error)
		}
		if update.RowsAffected == 1 {
			return domain.VerificationMethodRecoveryCode, 0, true, nil
		}
		return "", 0, false, nil
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return "", 0, false, fmt.Errorf("lock MFA recovery code: %w", result.Error)
	}
	counter, valid, err := verifier(factor, write.Code, write.Now)
	if err != nil {
		return "", 0, false, fmt.Errorf("verify MFA TOTP code: %w", err)
	}
	return domain.VerificationMethodTOTP, counter, valid, nil
}

func recordFailedAttempt(transaction *gorm.DB, challenge *challengeModel, verification *application.ChallengeVerification) error {
	attempts := challenge.AttemptCount + 1
	status := domain.ChallengeStatusPending
	if attempts >= challenge.MaxAttempts {
		status = domain.ChallengeStatusRejected
	}
	update := transaction.Model(&challengeModel{}).Where("id = ? AND status = ? AND attempt_count = ?", challenge.ID, domain.ChallengeStatusPending, challenge.AttemptCount).Updates(map[string]any{"attempt_count": attempts, "status": status})
	if update.Error != nil {
		return fmt.Errorf("record failed MFA challenge attempt: %w", update.Error)
	}
	if update.RowsAffected != 1 {
		return domain.ErrChallengeConsumed
	}
	challenge.AttemptCount, challenge.Status = attempts, status
	verification.AttemptsRemaining = challenge.MaxAttempts - attempts
	verification.Status = status
	return nil
}

func rejectUnavailableFactor(transaction *gorm.DB, challenge *challengeModel, verification *application.ChallengeVerification) error {
	update := transaction.Model(&challengeModel{}).Where("id = ? AND status = ?", challenge.ID, domain.ChallengeStatusPending).Updates(map[string]any{"status": domain.ChallengeStatusRejected})
	if update.Error != nil {
		return fmt.Errorf("reject MFA challenge for unavailable factor: %w", update.Error)
	}
	verification.Status = domain.ChallengeStatusRejected
	return domain.ErrFactorUnavailable
}

func toFactorModel(factor domain.TOTPFactor) totpFactorModel {
	return totpFactorModel{ID: factor.ID, TenantID: factor.TenantID, AccountID: factor.AccountID, DisplayName: factor.DisplayName, SecretCiphertext: append([]byte(nil), factor.SecretCiphertext...), EnrolledAt: factor.EnrolledAt, LastUsedAt: factor.LastUsedAt, LastAcceptedCounter: factor.LastAcceptedCounter, DisabledAt: factor.DisabledAt, ExpiresAt: factor.ExpiresAt, Status: factor.Status, CreatedAt: factor.CreatedAt, UpdatedAt: factor.UpdatedAt, Version: factor.Version}
}

func toFactor(row totpFactorModel) domain.TOTPFactor {
	return domain.TOTPFactor{ID: row.ID, TenantID: row.TenantID, AccountID: row.AccountID, DisplayName: row.DisplayName, SecretCiphertext: append([]byte(nil), row.SecretCiphertext...), EnrolledAt: copyTime(row.EnrolledAt), LastUsedAt: copyTime(row.LastUsedAt), LastAcceptedCounter: copyUint64(row.LastAcceptedCounter), DisabledAt: copyTime(row.DisabledAt), ExpiresAt: row.ExpiresAt, Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Version: row.Version}
}

func toChallengeModel(challenge domain.MFAChallenge) challengeModel {
	return challengeModel{ID: challenge.ID, TenantID: challenge.TenantID, AccountID: challenge.AccountID, FactorID: challenge.FactorID, ChallengeHash: append([]byte(nil), challenge.ChallengeHash[:]...), AttemptCount: challenge.AttemptCount, MaxAttempts: challenge.MaxAttempts, CreatedAt: challenge.CreatedAt, ExpiresAt: challenge.ExpiresAt, VerifiedAt: challenge.VerifiedAt, Status: challenge.Status}
}

func toRecoveryCodeModel(code domain.RecoveryCode) recoveryCodeModel {
	return recoveryCodeModel{ID: code.ID, TenantID: code.TenantID, FactorID: code.FactorID, CodeHash: append([]byte(nil), code.CodeHash[:]...), CreatedAt: code.CreatedAt, ConsumedAt: code.ConsumedAt}
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

type loginPreAuthenticationModel struct {
	ID             string     `gorm:"column:id"`
	TenantID       string     `gorm:"column:tenant_id"`
	AccountID      string     `gorm:"column:account_id"`
	CredentialHash []byte     `gorm:"column:credential_hash"`
	AttemptCount   uint16     `gorm:"column:attempt_count"`
	MaxAttempts    uint16     `gorm:"column:max_attempts"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	ExpiresAt      time.Time  `gorm:"column:expires_at"`
	ConsumedAt     *time.Time `gorm:"column:consumed_at"`
	Status         string     `gorm:"column:status"`
}

func (loginPreAuthenticationModel) TableName() string { return "iam_mfa_login_pre_auth" }

// CreateLoginPreAuthentication persists a credential only when the password-authenticated account
// still has at least one active TOTP factor. The opaque credential is represented by a SHA-256 hash.
func (repository *Repository) CreateLoginPreAuthentication(ctx context.Context, preAuth domain.LoginPreAuthentication) (bool, error) {
	var required bool
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var factor totpFactorModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND disabled_at IS NULL AND expires_at > ?", preAuth.TenantID, preAuth.AccountID, domain.FactorStatusActive, preAuth.CreatedAt.UTC()).
			First(&factor)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock active MFA factor for login pre-authentication: %w", result.Error)
		}
		row := toLoginPreAuthenticationModel(preAuth)
		if err := transaction.Create(&row).Error; err != nil {
			return fmt.Errorf("create MFA login pre-authentication: %w", err)
		}
		required = true
		return nil
	})
	return required, err
}

// VerifyLoginPreAuthentication verifies against any active factor bound to the persisted account.
// It locks pre-authentication, factors and recovery codes in one transaction so consumption, counter
// advancement and failed-attempt accounting cannot race.
func (repository *Repository) VerifyLoginPreAuthentication(ctx context.Context, write application.VerifyLoginPreAuthenticationWrite, verifier application.TOTPVerifier) (application.LoginPreAuthenticationVerification, error) {
	var verification application.LoginPreAuthenticationVerification
	var terminalErr error
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var preAuth loginPreAuthenticationModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("credential_hash = ?", write.CredentialHash[:]).First(&preAuth)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			terminalErr = domain.ErrLoginPreAuthenticationNotFound
			return nil
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA login pre-authentication: %w", result.Error)
		}
		verification = application.LoginPreAuthenticationVerification{Status: preAuth.Status}
		if preAuth.Status == domain.LoginPreAuthenticationStatusConsumed {
			terminalErr = domain.ErrLoginPreAuthenticationConsumed
			return nil
		}
		if preAuth.Status == domain.LoginPreAuthenticationStatusExpired || !write.Now.Before(preAuth.ExpiresAt) {
			if preAuth.Status == domain.LoginPreAuthenticationStatusPending {
				if err := transaction.Model(&loginPreAuthenticationModel{}).Where("id = ? AND status = ?", preAuth.ID, domain.LoginPreAuthenticationStatusPending).Updates(map[string]any{"status": domain.LoginPreAuthenticationStatusExpired}).Error; err != nil {
					return fmt.Errorf("expire MFA login pre-authentication: %w", err)
				}
			}
			verification.Status = domain.LoginPreAuthenticationStatusExpired
			terminalErr = domain.ErrLoginPreAuthenticationExpired
			return nil
		}
		if preAuth.Status != domain.LoginPreAuthenticationStatusPending {
			terminalErr = domain.ErrLoginPreAuthenticationAttemptsExceeded
			return nil
		}

		identity, err := findActiveLoginIdentity(transaction, preAuth.TenantID, preAuth.AccountID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := rejectLoginPreAuthentication(transaction, &preAuth, &verification); err != nil {
					return err
				}
				terminalErr = domain.ErrFactorUnavailable
				return nil
			}
			return err
		}

		var factors []totpFactorModel
		result = transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND account_id = ? AND status = ? AND disabled_at IS NULL AND expires_at > ?", preAuth.TenantID, preAuth.AccountID, domain.FactorStatusActive, write.Now.UTC()).
			Find(&factors)
		if result.Error != nil {
			return fmt.Errorf("lock active MFA factors for login pre-authentication: %w", result.Error)
		}
		if len(factors) == 0 {
			if err := rejectLoginPreAuthentication(transaction, &preAuth, &verification); err != nil {
				return err
			}
			terminalErr = domain.ErrFactorUnavailable
			return nil
		}

		for _, factor := range factors {
			method, counter, verified, err := verifyLoginCodeInTransaction(transaction, toFactor(factor), write, verifier)
			if err != nil {
				return err
			}
			if !verified {
				continue
			}
			if method == domain.VerificationMethodTOTP {
				usedAt := write.Now
				update := transaction.Model(&totpFactorModel{}).
					Where("id = ? AND status = ? AND (last_accepted_counter IS NULL OR last_accepted_counter < ?)", factor.ID, domain.FactorStatusActive, counter).
					Updates(map[string]any{"last_accepted_counter": counter, "last_used_at": usedAt, "updated_at": usedAt, "version": gorm.Expr("version + 1")})
				if update.Error != nil {
					return fmt.Errorf("advance MFA TOTP counter for login pre-authentication: %w", update.Error)
				}
				if update.RowsAffected != 1 {
					continue
				}
			}
			consumedAt := write.Now
			update := transaction.Model(&loginPreAuthenticationModel{}).Where("id = ? AND status = ?", preAuth.ID, domain.LoginPreAuthenticationStatusPending).
				Updates(map[string]any{"status": domain.LoginPreAuthenticationStatusConsumed, "consumed_at": consumedAt})
			if update.Error != nil {
				return fmt.Errorf("consume MFA login pre-authentication: %w", update.Error)
			}
			if update.RowsAffected != 1 {
				terminalErr = domain.ErrLoginPreAuthenticationConsumed
				return nil
			}
			verification = application.LoginPreAuthenticationVerification{
				Verified: true, VerificationMethod: method, AttemptsRemaining: preAuth.MaxAttempts - preAuth.AttemptCount,
				VerifiedAt: &consumedAt, Status: domain.LoginPreAuthenticationStatusConsumed, Identity: identity,
			}
			return nil
		}
		return recordFailedLoginPreAuthenticationAttempt(transaction, &preAuth, &verification)
	})
	if err != nil {
		return application.LoginPreAuthenticationVerification{}, err
	}
	if terminalErr != nil {
		return verification, terminalErr
	}
	return verification, nil
}

type loginIdentityProjection struct {
	TenantID    string `gorm:"column:tenant_id"`
	TenantName  string `gorm:"column:tenant_name"`
	TenantCode  string `gorm:"column:tenant_code"`
	UserID      string `gorm:"column:user_id"`
	UserName    string `gorm:"column:user_name"`
	AccountID   string `gorm:"column:account_id"`
	AccountName string `gorm:"column:account_name"`
}

func findActiveLoginIdentity(transaction *gorm.DB, tenantID, accountID string) (domain.LoginPreAuthenticationIdentity, error) {
	var row loginIdentityProjection
	result := transaction.Table("iam_account AS account").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code,
			user.id AS user_id, user.display_name AS user_name, account.id AS account_id,
			COALESCE(account.username, account.id) AS account_name`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = account.tenant_id AND tenant.status = ?", "ACTIVE").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id AND user.status = ?", "ACTIVE").
		Where("account.id = ? AND account.tenant_id = ? AND account.status = ?", accountID, tenantID, "ACTIVE").First(&row)
	if result.Error != nil {
		return domain.LoginPreAuthenticationIdentity{}, result.Error
	}
	return domain.LoginPreAuthenticationIdentity{TenantID: row.TenantID, TenantName: row.TenantName, TenantCode: row.TenantCode, UserID: row.UserID, UserName: row.UserName, AccountID: row.AccountID, AccountName: row.AccountName}, nil
}

func verifyLoginCodeInTransaction(transaction *gorm.DB, factor domain.TOTPFactor, write application.VerifyLoginPreAuthenticationWrite, verifier application.TOTPVerifier) (string, uint64, bool, error) {
	var recovery recoveryCodeModel
	result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("factor_id = ? AND code_hash = ? AND consumed_at IS NULL", factor.ID, write.CodeHash[:]).First(&recovery)
	if result.Error == nil {
		consumedAt := write.Now
		update := transaction.Model(&recoveryCodeModel{}).Where("id = ? AND consumed_at IS NULL", recovery.ID).Updates(map[string]any{"consumed_at": consumedAt})
		if update.Error != nil {
			return "", 0, false, fmt.Errorf("consume MFA recovery code for login pre-authentication: %w", update.Error)
		}
		return domain.VerificationMethodRecoveryCode, 0, update.RowsAffected == 1, nil
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return "", 0, false, fmt.Errorf("lock MFA recovery code for login pre-authentication: %w", result.Error)
	}
	counter, valid, err := verifier(factor, write.Code, write.Now)
	if err != nil {
		return "", 0, false, fmt.Errorf("verify MFA TOTP code for login pre-authentication: %w", err)
	}
	return domain.VerificationMethodTOTP, counter, valid, nil
}

func recordFailedLoginPreAuthenticationAttempt(transaction *gorm.DB, preAuth *loginPreAuthenticationModel, verification *application.LoginPreAuthenticationVerification) error {
	attempts := preAuth.AttemptCount + 1
	status := domain.LoginPreAuthenticationStatusPending
	if attempts >= preAuth.MaxAttempts {
		status = domain.LoginPreAuthenticationStatusRejected
	}
	update := transaction.Model(&loginPreAuthenticationModel{}).
		Where("id = ? AND status = ? AND attempt_count = ?", preAuth.ID, domain.LoginPreAuthenticationStatusPending, preAuth.AttemptCount).
		Updates(map[string]any{"attempt_count": attempts, "status": status})
	if update.Error != nil {
		return fmt.Errorf("record failed MFA login pre-authentication attempt: %w", update.Error)
	}
	if update.RowsAffected != 1 {
		return domain.ErrLoginPreAuthenticationConsumed
	}
	preAuth.AttemptCount, preAuth.Status = attempts, status
	verification.AttemptsRemaining = preAuth.MaxAttempts - attempts
	verification.Status = status
	return nil
}

func rejectLoginPreAuthentication(transaction *gorm.DB, preAuth *loginPreAuthenticationModel, verification *application.LoginPreAuthenticationVerification) error {
	update := transaction.Model(&loginPreAuthenticationModel{}).Where("id = ? AND status = ?", preAuth.ID, domain.LoginPreAuthenticationStatusPending).
		Updates(map[string]any{"status": domain.LoginPreAuthenticationStatusRejected})
	if update.Error != nil {
		return fmt.Errorf("reject MFA login pre-authentication for unavailable factor: %w", update.Error)
	}
	if update.RowsAffected != 1 {
		return domain.ErrLoginPreAuthenticationConsumed
	}
	verification.Status = domain.LoginPreAuthenticationStatusRejected
	return nil
}

func toLoginPreAuthenticationModel(preAuth domain.LoginPreAuthentication) loginPreAuthenticationModel {
	return loginPreAuthenticationModel{ID: preAuth.ID, TenantID: preAuth.TenantID, AccountID: preAuth.AccountID, CredentialHash: append([]byte(nil), preAuth.CredentialHash[:]...), AttemptCount: preAuth.AttemptCount, MaxAttempts: preAuth.MaxAttempts, CreatedAt: preAuth.CreatedAt, ExpiresAt: preAuth.ExpiresAt, ConsumedAt: preAuth.ConsumedAt, Status: preAuth.Status}
}
