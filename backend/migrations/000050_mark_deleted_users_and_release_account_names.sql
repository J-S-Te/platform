-- Distinguish a business-deleted user from an ordinarily disabled user. Physical deletion is not
-- used because sessions, audit/security records and protocol grants retain restrictive foreign keys.
ALTER TABLE iam_user
    ADD COLUMN deleted_at DATETIME(3) NULL AFTER status,
    ADD COLUMN deleted_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER deleted_at,
    ADD KEY idx_user_tenant_deleted (tenant_id, deleted_at);

-- Backfill users deleted by the earlier logical-delete implementation. The signature mirrors that
-- transaction: all related accounts and memberships are disabled and at least one account remains.
UPDATE iam_user AS u
SET u.deleted_at = u.updated_at,
    u.deleted_by = u.updated_by
WHERE u.deleted_at IS NULL
  AND u.status = 'DISABLED'
  AND EXISTS (
      SELECT 1 FROM iam_account AS historical_account
      WHERE historical_account.tenant_id = u.tenant_id
        AND historical_account.user_id = u.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM iam_account AS active_account
      WHERE active_account.tenant_id = u.tenant_id
        AND active_account.user_id = u.id
        AND active_account.status <> 'DISABLED'
  )
  AND NOT EXISTS (
      SELECT 1 FROM iam_membership AS active_membership
      WHERE active_membership.tenant_id = u.tenant_id
        AND active_membership.user_id = u.id
        AND active_membership.status <> 'DISABLED'
  );

-- Release unique tenant/account names while retaining the immutable account row for audit and FK
-- integrity. Historical audit events keep their event-time account-name snapshot.
UPDATE iam_account AS account
JOIN iam_user AS deleted_user
  ON deleted_user.id = account.user_id
 AND deleted_user.tenant_id = account.tenant_id
SET account.username = NULL,
    account.updated_at = UTC_TIMESTAMP(3)
WHERE deleted_user.deleted_at IS NOT NULL
  AND account.username IS NOT NULL;
