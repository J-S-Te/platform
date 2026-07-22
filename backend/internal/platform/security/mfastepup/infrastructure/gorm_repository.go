// Package infrastructure implements MySQL persistence for MFA step-up grants with GORM.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository persists only digests of opaque MFA step-up challenges and grants. It uses explicit
// transactions and row locks for lifecycle transitions; it never calls AutoMigrate.
type Repository struct {
	database *gorm.DB
}

// NewRepository constructs the GORM adapter.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("MFA step-up GORM database must not be nil")
	}
	return &Repository{database: database}, nil
}

type grantModel struct {
	ID                 string     `gorm:"column:id"`
	TenantID           string     `gorm:"column:tenant_id"`
	AccountID          string     `gorm:"column:account_id"`
	SessionID          string     `gorm:"column:session_id"`
	MFAChallengeID     string     `gorm:"column:mfa_challenge_id"`
	ChallengeHash      []byte     `gorm:"column:challenge_hash"`
	GrantHash          []byte     `gorm:"column:grant_hash"`
	ChallengeExpiresAt time.Time  `gorm:"column:challenge_expires_at"`
	GrantExpiresAt     *time.Time `gorm:"column:grant_expires_at"`
	GrantedAt          *time.Time `gorm:"column:granted_at"`
	ConsumedAt         *time.Time `gorm:"column:consumed_at"`
	Status             string     `gorm:"column:status"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
}

func (grantModel) TableName() string { return "sec_mfa_step_up_grant" }

// Create writes the session binding for a newly created MFA challenge.
func (repository *Repository) Create(ctx context.Context, grant domain.Grant) error {
	row := toModel(grant)
	return repository.database.WithContext(ctx).Create(&row).Error
}

// AuthorizeChallenge verifies a persisted challenge digest belongs to the exact current session.
// It does not consume the record: the identity MFA transaction remains the sole verifier and
// guarantee that a TOTP/recovery-code challenge can be successfully verified only once.
func (repository *Repository) AuthorizeChallenge(ctx context.Context, authorization application.ChallengeAuthorization) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row grantModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("challenge_hash = ?", authorization.ChallengeHash[:]).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return domain.ErrChallengeNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA step-up challenge: %w", result.Error)
		}
		if row.TenantID != authorization.TenantID || row.AccountID != authorization.AccountID || row.SessionID != authorization.SessionID {
			return domain.ErrChallengeBinding
		}
		if row.Status == domain.GrantStatusExpired || !authorization.Now.Before(row.ChallengeExpiresAt) {
			if row.Status != domain.GrantStatusExpired {
				if err := transaction.Model(&grantModel{}).Where("id = ? AND status = ?", row.ID, domain.GrantStatusPending).Update("status", domain.GrantStatusExpired).Error; err != nil {
					return fmt.Errorf("expire MFA step-up challenge: %w", err)
				}
			}
			return domain.ErrChallengeExpired
		}
		if row.Status != domain.GrantStatusPending {
			return domain.ErrChallengeNotPending
		}
		return nil
	})
}

// IssueGrant transitions a verified pending challenge into a short-lived grant while holding the
// row lock. A duplicate/racing issuer cannot replace the first grant.
func (repository *Repository) IssueGrant(ctx context.Context, write application.IssueGrantWrite) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row grantModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("challenge_hash = ?", write.ChallengeHash[:]).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return domain.ErrChallengeNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock verified MFA step-up challenge: %w", result.Error)
		}
		if row.TenantID != write.TenantID || row.AccountID != write.AccountID || row.SessionID != write.SessionID {
			return domain.ErrChallengeBinding
		}
		if row.Status == domain.GrantStatusExpired || !write.GrantedAt.Before(row.ChallengeExpiresAt) {
			if row.Status == domain.GrantStatusPending {
				if err := transaction.Model(&grantModel{}).Where("id = ? AND status = ?", row.ID, domain.GrantStatusPending).Update("status", domain.GrantStatusExpired).Error; err != nil {
					return fmt.Errorf("expire verified MFA step-up challenge: %w", err)
				}
			}
			return domain.ErrChallengeExpired
		}
		if row.Status != domain.GrantStatusPending {
			return domain.ErrChallengeNotPending
		}
		result = transaction.Model(&grantModel{}).Where("id = ? AND status = ?", row.ID, domain.GrantStatusPending).Updates(map[string]any{
			"grant_hash":       write.GrantHash[:],
			"granted_at":       write.GrantedAt,
			"grant_expires_at": write.GrantExpiresAt,
			"status":           domain.GrantStatusIssued,
		})
		if result.Error != nil {
			return fmt.Errorf("issue MFA step-up grant: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.ErrChallengeNotPending
		}
		return nil
	})
}

// ConsumeGrant locks a digest-matched grant and transitions it to CONSUMED exactly once. The
// tenant, account and session checks are performed under the same transaction as consumption.
func (repository *Repository) ConsumeGrant(ctx context.Context, write application.ConsumeGrantWrite) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row grantModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("grant_hash = ?", write.GrantHash[:]).First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return domain.ErrGrantNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock MFA step-up grant: %w", result.Error)
		}
		if row.TenantID != write.TenantID || row.AccountID != write.AccountID || row.SessionID != write.SessionID {
			return domain.ErrGrantBinding
		}
		if row.Status == domain.GrantStatusConsumed || row.ConsumedAt != nil {
			return domain.ErrGrantConsumed
		}
		if row.Status == domain.GrantStatusExpired || row.GrantExpiresAt == nil || !write.Now.Before(*row.GrantExpiresAt) {
			if row.Status == domain.GrantStatusIssued {
				if err := transaction.Model(&grantModel{}).Where("id = ? AND status = ?", row.ID, domain.GrantStatusIssued).Update("status", domain.GrantStatusExpired).Error; err != nil {
					return fmt.Errorf("expire MFA step-up grant: %w", err)
				}
			}
			return domain.ErrGrantExpired
		}
		if row.Status != domain.GrantStatusIssued {
			return domain.ErrGrantNotIssued
		}
		result = transaction.Model(&grantModel{}).Where("id = ? AND status = ? AND consumed_at IS NULL", row.ID, domain.GrantStatusIssued).Updates(map[string]any{"status": domain.GrantStatusConsumed, "consumed_at": write.Now})
		if result.Error != nil {
			return fmt.Errorf("consume MFA step-up grant: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.ErrGrantConsumed
		}
		return nil
	})
}

func toModel(grant domain.Grant) grantModel {
	row := grantModel{
		ID: grant.ID, TenantID: grant.TenantID, AccountID: grant.AccountID, SessionID: grant.SessionID,
		MFAChallengeID: grant.MFAChallengeID, ChallengeHash: append([]byte(nil), grant.ChallengeHash[:]...),
		ChallengeExpiresAt: grant.ChallengeExpiresAt, GrantExpiresAt: grant.GrantExpiresAt,
		GrantedAt: grant.GrantedAt, ConsumedAt: grant.ConsumedAt, Status: grant.Status, CreatedAt: grant.CreatedAt,
	}
	if grant.GrantHash != nil {
		row.GrantHash = append([]byte(nil), grant.GrantHash[:]...)
	}
	return row
}
