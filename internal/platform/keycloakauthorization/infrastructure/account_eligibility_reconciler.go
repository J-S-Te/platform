package infrastructure

import (
	"context"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

type eligibilityCandidate struct {
	TenantID, IdentityID, ApplicationID, EnvironmentID string
}

// AccountEligibilityReconciler periodically creates normal projection events
// when a user's effective login eligibility differs from Keycloak's last
// projected enabled flag. It never writes Keycloak directly.
type AccountEligibilityReconciler struct{ database *gorm.DB }

func NewAccountEligibilityReconciler(database *gorm.DB) (*AccountEligibilityReconciler, error) {
	if database == nil {
		return nil, fmt.Errorf("Keycloak account eligibility reconciler database must not be nil")
	}
	return &AccountEligibilityReconciler{database: database}, nil
}

func (reconciler *AccountEligibilityReconciler) Reconcile(ctx context.Context, now time.Time) error {
	now = now.UTC()
	return reconciler.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []eligibilityCandidate
		if err := tx.Raw(`
SELECT projection.tenant_id, projection.identity_id, projection.application_id, projection.environment_id
FROM keycloak_authorization_projection AS projection
JOIN iam_user AS user_record ON user_record.tenant_id = projection.tenant_id AND user_record.id = projection.identity_id
WHERE projection.status = 'SYNCED'
  AND projection.user_enabled <> (
    user_record.status = 'ACTIVE' AND user_record.deleted_at IS NULL AND EXISTS (
      SELECT 1 FROM iam_account AS account
      WHERE account.tenant_id = user_record.tenant_id AND account.user_id = user_record.id
        AND account.status = 'ACTIVE'
        AND (account.valid_until IS NULL OR account.valid_until > ?)
        AND (account.locked_until IS NULL OR account.locked_until <= ?)
    )
  )
  AND NOT EXISTS (
    SELECT 1 FROM keycloak_authorization_outbox AS pending
    WHERE pending.tenant_id = projection.tenant_id AND pending.identity_id = projection.identity_id
      AND pending.application_id = projection.application_id AND pending.environment_id = projection.environment_id
      AND pending.status IN ('PENDING', 'RUNNING')
  )`, now, now).Scan(&candidates).Error; err != nil {
			return fmt.Errorf("find Keycloak eligibility changes: %w", err)
		}
		for _, candidate := range candidates {
			id, err := (ulid.Generator{}).New(now)
			if err != nil {
				return fmt.Errorf("generate Keycloak eligibility event ID: %w", err)
			}
			if err := tx.Table("keycloak_authorization_outbox").Create(map[string]any{
				"id": id, "tenant_id": candidate.TenantID, "identity_id": candidate.IdentityID,
				"application_id": candidate.ApplicationID, "environment_id": candidate.EnvironmentID,
				"event_type": "IDENTITY_CHANGED", "authorization_revision": 0, "status": "PENDING",
				"available_at": now, "attempts": 0, "created_at": now,
			}).Error; err != nil {
				return fmt.Errorf("enqueue Keycloak eligibility event: %w", err)
			}
		}
		return nil
	})
}
