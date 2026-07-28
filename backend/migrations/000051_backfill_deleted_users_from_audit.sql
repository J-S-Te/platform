-- Migration 000050 could identify legacy deleted users that still owned accounts. Account-less users
-- require the authoritative successful DELETE audit event so ordinarily disabled users are not
-- misclassified as deleted.
UPDATE iam_user AS user
JOIN (
    SELECT tenant_id, resource_id AS user_id, MAX(occurred_at) AS deleted_at
    FROM audit_event
    WHERE action = 'DELETE /api/v1/users/:user_id'
      AND resource_type = 'IDENTITY'
      AND result = 'SUCCESS'
      AND resource_id IS NOT NULL
    GROUP BY tenant_id, resource_id
) AS deletion
  ON deletion.tenant_id = user.tenant_id
 AND deletion.user_id = user.id
SET user.deleted_at = deletion.deleted_at
WHERE user.deleted_at IS NULL
  AND user.status = 'DISABLED';

-- Release names for any account attached to a user newly identified by the audit backfill.
UPDATE iam_account AS account
JOIN iam_user AS deleted_user
  ON deleted_user.id = account.user_id
 AND deleted_user.tenant_id = account.tenant_id
SET account.username = NULL,
    account.status = 'DISABLED',
    account.updated_at = UTC_TIMESTAMP(3)
WHERE deleted_user.deleted_at IS NOT NULL
  AND account.username IS NOT NULL;
