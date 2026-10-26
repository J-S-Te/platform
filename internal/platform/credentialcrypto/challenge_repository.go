package credentialcrypto

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

type ChallengeGORMRepository struct{ DB *gorm.DB }

func NewChallengeGORMRepository(db *gorm.DB) (*ChallengeGORMRepository, error) {
	if db == nil {
		return nil, errors.New("credential challenge repository requires database")
	}
	return &ChallengeGORMRepository{DB: db}, nil
}

type challengeModel struct {
	IDHash      []byte     `gorm:"column:id_hash"`
	KID         string     `gorm:"column:kid"`
	Purpose     string     `gorm:"column:purpose"`
	ResourceID  string     `gorm:"column:resource_id"`
	TenantID    *string    `gorm:"column:tenant_id"`
	BindingHash []byte     `gorm:"column:binding_hash"`
	HTTPMethod  string     `gorm:"column:http_method"`
	RouteKey    string     `gorm:"column:route_key"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	ConsumedAt  *time.Time `gorm:"column:consumed_at"`
}

func (challengeModel) TableName() string { return "credential_encryption_challenge" }

func (repository *ChallengeGORMRepository) Create(ctx context.Context, challenge Challenge) error {
	var tenant *string
	if challenge.TenantID != "" {
		value := challenge.TenantID
		tenant = &value
	}
	row := challengeModel{
		IDHash: challenge.IDHash, KID: challenge.KID, Purpose: challenge.Purpose,
		ResourceID: challenge.ResourceID, TenantID: tenant, BindingHash: challenge.BindingHash,
		HTTPMethod: challenge.HTTPMethod, RouteKey: challenge.RouteKey,
		CreatedAt: challenge.CreatedAt, ExpiresAt: challenge.ExpiresAt,
	}
	return repository.DB.WithContext(ctx).Create(&row).Error
}

func (repository *ChallengeGORMRepository) Consume(ctx context.Context, challenge Challenge) error {
	var tenant any
	if challenge.TenantID != "" {
		tenant = challenge.TenantID
	}
	result := repository.DB.WithContext(ctx).Exec(`
UPDATE credential_encryption_challenge
SET consumed_at = UTC_TIMESTAMP(3)
WHERE id_hash = ? AND binding_hash = ? AND kid = ? AND purpose = ? AND resource_id = ?
  AND http_method = 'POST' AND route_key = ?
  AND ((tenant_id = ?) OR (tenant_id IS NULL AND ? IS NULL))
  AND consumed_at IS NULL AND expires_at > UTC_TIMESTAMP(3)`,
		challenge.IDHash, challenge.BindingHash, challenge.KID, challenge.Purpose, challenge.ResourceID,
		challenge.RouteKey, tenant, tenant)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var row struct {
		ConsumedAt *time.Time `gorm:"column:consumed_at"`
		ExpiresAt  time.Time  `gorm:"column:expires_at"`
	}
	err := repository.DB.WithContext(ctx).Raw(`SELECT consumed_at,expires_at FROM credential_encryption_challenge WHERE id_hash = ? AND binding_hash = ? AND kid = ? AND purpose = ? AND resource_id = ? AND route_key = ? AND ((tenant_id = ?) OR (tenant_id IS NULL AND ? IS NULL)) LIMIT 1`,
		challenge.IDHash, challenge.BindingHash, challenge.KID, challenge.Purpose, challenge.ResourceID, challenge.RouteKey, tenant, tenant).Scan(&row).Error
	if err != nil {
		return err
	}
	if row.ExpiresAt.IsZero() {
		return ErrChallengeInvalid
	}
	if row.ConsumedAt != nil {
		return ErrChallengeUsed
	}
	if !row.ExpiresAt.After(time.Now().UTC()) {
		return ErrChallengeExpired
	}
	return ErrChallengeInvalid
}

func (repository *ChallengeGORMRepository) DeleteExpired(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > MaxCleanupBatch {
		limit = MaxCleanupBatch
	}
	result := repository.DB.WithContext(ctx).Exec(`
DELETE FROM credential_encryption_challenge
WHERE expires_at < ? ORDER BY expires_at LIMIT ?`, cutoff, limit)
	return result.RowsAffected, result.Error
}
