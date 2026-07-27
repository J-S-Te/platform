package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateLocalAccount creates one active local human account and its password credential in one
// transaction. A tenant-scoped active user is required so arbitrary identities cannot be linked.
func (repository *GORMRepository) CreateLocalAccount(ctx context.Context, write application.LocalAccountCreateWrite) (domain.Account, error) {
	var account domain.Account
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var user userModel
		result := transaction.Where("tenant_id = ? AND id = ? AND status = ?", write.TenantID, write.UserID, domain.StatusActive).First(&user)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("get account user: %w", result.Error)
		}
		accountName := write.AccountName
		operatorID := write.OperatorID
		if err := transaction.Create(&accountModel{ID: write.AccountID, TenantID: write.TenantID, UserID: &write.UserID, Username: &accountName, AccountType: "HUMAN", AuthSource: "LOCAL", Status: domain.StatusActive, ValidUntil: copyTime(write.ValidUntil), Version: 1, CreatedAt: write.OccurredAt, CreatedBy: &operatorID, UpdatedAt: write.OccurredAt, UpdatedBy: &operatorID}).Error; err != nil {
			return mapWriteError(err, "create local account")
		}
		if err := transaction.Create(&passwordCredentialModel{ID: write.CredentialID, AccountID: write.AccountID, PasswordHash: append([]byte(nil), write.PasswordDigest...), HashAlgorithm: "argon2id", AlgorithmParams: append([]byte(nil), write.AlgorithmParams...), MustChange: false, FailedAttempts: 0, Status: domain.StatusActive, PasswordChangedAt: write.OccurredAt, CreatedAt: write.OccurredAt, UpdatedAt: write.OccurredAt}).Error; err != nil {
			return mapWriteError(err, "create local account credential")
		}
		account = domain.Account{ID: write.AccountID, TenantID: write.TenantID, UserID: &write.UserID, AccountName: write.AccountName, Status: domain.StatusActive, ValidUntil: copyTime(write.ValidUntil), Version: 1, CreatedAt: write.OccurredAt, UpdatedAt: write.OccurredAt}
		return nil
	})
	if err != nil {
		return domain.Account{}, err
	}
	return account, nil
}

// InitializePassword creates a password credential only for an account that does not have one.
func (repository *GORMRepository) InitializePassword(ctx context.Context, write application.PasswordWrite) (domain.Account, error) {
	return repository.writeAdministratorPassword(ctx, write, true)
}

// ResetPassword replaces an existing local password credential without unlocking a locked account.
func (repository *GORMRepository) ResetPassword(ctx context.Context, write application.PasswordWrite) (domain.Account, error) {
	return repository.writeAdministratorPassword(ctx, write, false)
}

func (repository *GORMRepository) writeAdministratorPassword(ctx context.Context, write application.PasswordWrite, initialize bool) (domain.Account, error) {
	var account domain.Account
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row accountModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", write.TenantID, write.AccountID).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock account password update: %w", result.Error)
		}
		if row.Version != write.ExpectedVersion {
			return application.ErrVersionConflict
		}
		if row.AccountType != "HUMAN" || row.AuthSource != "LOCAL" {
			return application.ErrConflict
		}
		// Initializing a missing password is allowed only for active accounts. An administrator may
		// reset a locked account to deliver a new password offline, but the reset must not unlock it.
		if initialize && row.Status != domain.StatusActive {
			return application.ErrConflict
		}
		if !initialize && row.Status != domain.StatusActive && row.Status != domain.AccountStatusLocked {
			return application.ErrConflict
		}

		var credential passwordCredentialModel
		credentialResult := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ?", write.AccountID).First(&credential)
		if initialize {
			if credentialResult.Error == nil {
				return application.ErrConflict
			}
			if !errors.Is(credentialResult.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("check password credential: %w", credentialResult.Error)
			}
			if err := transaction.Create(&passwordCredentialModel{ID: write.CredentialID, AccountID: write.AccountID, PasswordHash: append([]byte(nil), write.PasswordDigest...), HashAlgorithm: "argon2id", AlgorithmParams: append([]byte(nil), write.AlgorithmParams...), MustChange: false, FailedAttempts: 0, Status: domain.StatusActive, PasswordChangedAt: write.OccurredAt, CreatedAt: write.OccurredAt, UpdatedAt: write.OccurredAt}).Error; err != nil {
				return mapWriteError(err, "initialize password credential")
			}
		} else {
			if errors.Is(credentialResult.Error, gorm.ErrRecordNotFound) {
				return application.ErrConflict
			}
			if credentialResult.Error != nil {
				return fmt.Errorf("lock password credential: %w", credentialResult.Error)
			}
			if credential.Status != domain.StatusActive {
				return application.ErrConflict
			}
			if err := transaction.Model(&passwordCredentialModel{}).Where("id = ?", credential.ID).Updates(map[string]any{"password_hash": append([]byte(nil), write.PasswordDigest...), "hash_algorithm": "argon2id", "algorithm_params": append([]byte(nil), write.AlgorithmParams...), "must_change": false, "failed_attempts": 0, "last_failed_at": nil, "password_changed_at": write.OccurredAt, "updated_at": write.OccurredAt}).Error; err != nil {
				return fmt.Errorf("reset password credential: %w", err)
			}
		}
		operatorID := write.OperatorID
		accountUpdate := transaction.Model(&accountModel{}).Where("tenant_id = ? AND id = ? AND version = ?", write.TenantID, write.AccountID, write.ExpectedVersion).Updates(map[string]any{"updated_at": write.OccurredAt, "updated_by": &operatorID, "version": gorm.Expr("version + 1")})
		if accountUpdate.Error != nil {
			return fmt.Errorf("update account password version: %w", accountUpdate.Error)
		}
		if accountUpdate.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		if err := revokeActiveSessions(transaction, write); err != nil {
			return err
		}
		row.Version++
		row.UpdatedAt = write.OccurredAt
		account = toAccountWithFallbackName(row)
		return nil
	})
	if err != nil {
		return domain.Account{}, err
	}
	return account, nil
}

// FindLocalPasswordCredential reads a credential for self-service password verification.
func (repository *GORMRepository) FindLocalPasswordCredential(ctx context.Context, tenantID, accountID string) (application.LocalPasswordCredential, error) {
	var account accountModel
	result := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ? AND account_type = ? AND auth_source = ?", tenantID, accountID, "HUMAN", "LOCAL").First(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return application.LocalPasswordCredential{}, application.ErrNotFound
	}
	if result.Error != nil {
		return application.LocalPasswordCredential{}, fmt.Errorf("get local account credential: %w", result.Error)
	}
	var credential passwordCredentialModel
	result = repository.database.WithContext(ctx).Where("account_id = ?", accountID).First(&credential)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return application.LocalPasswordCredential{}, application.ErrConflict
	}
	if result.Error != nil {
		return application.LocalPasswordCredential{}, fmt.Errorf("get password credential: %w", result.Error)
	}
	return application.LocalPasswordCredential{TenantID: tenantID, AccountID: accountID, AccountStatus: account.Status, CredentialStatus: credential.Status, PasswordHash: append([]byte(nil), credential.PasswordHash...), HashAlgorithm: credential.HashAlgorithm, AlgorithmParams: append([]byte(nil), credential.AlgorithmParams...)}, nil
}

// ChangeOwnPassword uses compare-and-set on the previous digest to prevent a concurrent password
// change from silently replacing a newer secret. It revokes all active sessions atomically.
func (repository *GORMRepository) ChangeOwnPassword(ctx context.Context, write application.PasswordWrite) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		result := transaction.Model(&passwordCredentialModel{}).Where("account_id = ? AND password_hash = ? AND status = ?", write.AccountID, write.ExpectedHash, domain.StatusActive).Updates(map[string]any{"password_hash": append([]byte(nil), write.PasswordDigest...), "hash_algorithm": "argon2id", "algorithm_params": append([]byte(nil), write.AlgorithmParams...), "must_change": false, "failed_attempts": 0, "last_failed_at": nil, "password_changed_at": write.OccurredAt, "updated_at": write.OccurredAt})
		if result.Error != nil {
			return fmt.Errorf("change own password credential: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return application.ErrConflict
		}
		operatorID := write.OperatorID
		accountUpdate := transaction.Model(&accountModel{}).Where("tenant_id = ? AND id = ? AND status = ?", write.TenantID, write.AccountID, domain.StatusActive).Updates(map[string]any{"updated_at": write.OccurredAt, "updated_by": &operatorID, "version": gorm.Expr("version + 1")})
		if accountUpdate.Error != nil {
			return fmt.Errorf("update own password account: %w", accountUpdate.Error)
		}
		if accountUpdate.RowsAffected != 1 {
			return application.ErrConflict
		}
		return revokeActiveSessions(transaction, write)
	})
}

func revokeActiveSessions(transaction *gorm.DB, write application.PasswordWrite) error {
	reason := write.RevokeReason
	if err := transaction.Model(&sessionModel{}).Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", write.TenantID, write.AccountID, domain.StatusActive).Updates(map[string]any{"revoked_at": write.OccurredAt, "revoke_reason": &reason, "status": "REVOKED"}).Error; err != nil {
		return fmt.Errorf("revoke account sessions: %w", err)
	}
	return nil
}
