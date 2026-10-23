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

// AccountEligibilityReconciler 对账登录资格，并补偿角色/任职自然生效或到期。
// last_synced_at 是加载授权快照前的水位；跨过它的时间边界进入普通投影队列，
// 不过滤已到期的窗口，也不直接写 Keycloak。临时锁定由 IdP 自己管理。
type AccountEligibilityReconciler struct{ database *gorm.DB }

// NewAccountEligibilityReconciler 构造资格对账器；database 为空时返回错误。
func NewAccountEligibilityReconciler(database *gorm.DB) (*AccountEligibilityReconciler, error) {
	if database == nil {
		return nil, fmt.Errorf("Keycloak account eligibility reconciler database must not be nil")
	}
	return &AccountEligibilityReconciler{database: database}, nil
}

// Reconcile 在单个事务内扫描上次水位后发生资格变化（账号启停、角色/任职窗口
// 生效或到期、超管绑定变化）的已同步投影，为每个候选入队一条 IDENTITY_CHANGED
// 投影事件；同主体已有 PENDING/RUNNING 事件时跳过，避免重复排队。
func (reconciler *AccountEligibilityReconciler) Reconcile(ctx context.Context, now time.Time) error {
	now = now.UTC()
	return reconciler.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []eligibilityCandidate
		if err := tx.Raw(`
SELECT projection.tenant_id, projection.identity_id, projection.application_id, projection.environment_id
FROM keycloak_authorization_projection AS projection
JOIN iam_user AS user_record ON user_record.tenant_id = projection.tenant_id AND user_record.id = projection.identity_id
CROSS JOIN (SELECT ? AS now_utc) AS reconcile_clock
WHERE projection.status = 'SYNCED'
  AND EXISTS (
    SELECT 1 FROM keycloak_application_client_mapping AS mapping
    WHERE mapping.tenant_id = projection.tenant_id AND mapping.application_id = projection.application_id
      AND mapping.environment_id = projection.environment_id AND mapping.status = 'SYNCED'
  )
  AND (
    projection.last_synced_at IS NULL
    OR projection.user_enabled <> (
      user_record.status = 'ACTIVE' AND user_record.deleted_at IS NULL AND EXISTS (
        SELECT 1 FROM iam_account AS account
        WHERE account.tenant_id = user_record.tenant_id AND account.user_id = user_record.id
          AND account.status = 'ACTIVE'
          AND (account.valid_until IS NULL OR account.valid_until > reconcile_clock.now_utc)
      )
    )
    OR EXISTS (
      SELECT 1 FROM authz_role_binding AS timed_binding
      JOIN authz_role AS timed_role ON timed_role.id = timed_binding.role_id
        AND timed_role.tenant_id = timed_binding.tenant_id
        AND timed_role.application_id = timed_binding.application_id
        AND timed_role.status = 'ACTIVE'
      WHERE timed_binding.tenant_id = projection.tenant_id
        AND timed_binding.application_id = projection.application_id
        AND timed_binding.status = 'ACTIVE'
        AND ((timed_binding.scope_type = 'TENANT' AND timed_binding.scope_id = '')
          OR (timed_binding.scope_type = 'ENVIRONMENT' AND timed_binding.scope_id = projection.environment_id))
        AND ((timed_binding.subject_type = 'USER' AND timed_binding.subject_id = projection.identity_id)
          OR EXISTS (
            SELECT 1 FROM iam_membership AS role_membership
            WHERE role_membership.tenant_id = projection.tenant_id
              AND role_membership.user_id = projection.identity_id
              AND role_membership.status = 'ACTIVE' AND role_membership.inherit_authorization = 1
              AND ((timed_binding.subject_type = 'ORG_UNIT' AND timed_binding.subject_id = role_membership.org_unit_id)
                OR (timed_binding.subject_type = 'POSITION' AND timed_binding.subject_id = role_membership.position_id))
          ))
        AND ((timed_binding.valid_from > projection.last_synced_at AND timed_binding.valid_from <= reconcile_clock.now_utc)
          OR (timed_binding.valid_until > projection.last_synced_at AND timed_binding.valid_until <= reconcile_clock.now_utc))
    )
    OR EXISTS (
      SELECT 1 FROM iam_membership AS timed_membership
      WHERE timed_membership.tenant_id = projection.tenant_id
        AND timed_membership.user_id = projection.identity_id AND timed_membership.status = 'ACTIVE'
        AND ((timed_membership.valid_from > projection.last_synced_at AND timed_membership.valid_from <= reconcile_clock.now_utc)
          OR (timed_membership.valid_until > projection.last_synced_at AND timed_membership.valid_until <= reconcile_clock.now_utc))
    )
    OR EXISTS (
      SELECT 1 FROM authz_role_binding AS timed_admin_binding
      JOIN authz_role AS timed_admin_role ON timed_admin_role.id = timed_admin_binding.role_id
        AND timed_admin_role.tenant_id = timed_admin_binding.tenant_id
        AND timed_admin_role.application_id = timed_admin_binding.application_id
        AND timed_admin_role.status = 'ACTIVE' AND timed_admin_role.role_type = 'PLATFORM'
        AND timed_admin_role.code = 'platform-super-admin'
      JOIN platform_application AS admin_application ON admin_application.id = timed_admin_role.application_id
        AND admin_application.tenant_id = timed_admin_role.tenant_id
        AND admin_application.status = 'ACTIVE' AND admin_application.code = 'platform'
      WHERE timed_admin_binding.tenant_id = projection.tenant_id
        AND timed_admin_binding.subject_type = 'USER' AND timed_admin_binding.subject_id = projection.identity_id
        AND timed_admin_binding.scope_type = 'TENANT' AND timed_admin_binding.scope_id = ''
        AND timed_admin_binding.status = 'ACTIVE'
        AND ((timed_admin_binding.valid_from > projection.last_synced_at AND timed_admin_binding.valid_from <= reconcile_clock.now_utc)
          OR (timed_admin_binding.valid_until > projection.last_synced_at AND timed_admin_binding.valid_until <= reconcile_clock.now_utc))
    )
  )
  AND NOT EXISTS (
    SELECT 1 FROM keycloak_authorization_outbox AS pending
    WHERE pending.tenant_id = projection.tenant_id AND pending.identity_id = projection.identity_id
      AND pending.application_id = projection.application_id AND pending.environment_id = projection.environment_id
      AND pending.status IN ('PENDING', 'RUNNING')
  )`, now).Scan(&candidates).Error; err != nil {
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
