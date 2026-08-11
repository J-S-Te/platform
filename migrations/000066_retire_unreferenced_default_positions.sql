-- Stop exposing legacy fixed business positions that were copied into every organization by
-- migrations 000047/000049. New organization creation no longer provisions these positions.
--
-- This migration is intentionally conservative and repeatable:
--   * it only targets migration-owned position codes/IDs;
--   * it never deletes rows, preserving foreign keys and audit history;
--   * any position with an active membership, template assignment, or role binding remains active
--     so administrators can migrate that position through the normal management workflow.
UPDATE iam_position AS position
SET
    position.status = 'DISABLED',
    position.version = position.version + 1,
    position.updated_at = UTC_TIMESTAMP(3),
    position.updated_by = NULL
WHERE position.status = 'ACTIVE'
  AND (
      CONVERT(position.code USING utf8mb4) LIKE _utf8mb4'POS-DEFAULT-%'
      OR position.id IN (
          '01J00000000000000000000400',
          '01J00000000000000000000401',
          '01J00000000000000000000402',
          '01J00000000000000000000403',
          '01J00000000000000000000404',
          '01J00000000000000000000405'
      )
  )
  AND NOT EXISTS (
      SELECT 1
      FROM iam_membership AS membership
      WHERE membership.tenant_id = position.tenant_id
        AND membership.position_id = position.id
        AND membership.status = 'ACTIVE'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM authz_position_grant_template_assignment AS assignment
      WHERE assignment.tenant_id = position.tenant_id
        AND assignment.position_id = position.id
        AND assignment.status = 'ACTIVE'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM authz_role_binding AS binding
      WHERE binding.tenant_id = position.tenant_id
        AND binding.subject_type = 'POSITION'
        AND binding.subject_id = position.id
        AND binding.status = 'ACTIVE'
  );
