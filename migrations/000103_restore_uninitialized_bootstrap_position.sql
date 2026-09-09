-- Migration 000066 retires unused legacy positions before first-admin bootstrap.
-- Restore only the exact bootstrap seed in the uninitialized default tenant.
-- Do not reactivate other retired positions or override an initialized tenant's decisions.
UPDATE iam_position AS position
JOIN iam_tenant AS tenant ON tenant.id = position.tenant_id
JOIN iam_org_unit AS org ON org.id = position.org_unit_id AND org.tenant_id = tenant.id
SET position.status = 'ACTIVE',
    position.version = position.version + 1,
    position.updated_at = UTC_TIMESTAMP(3),
    position.updated_by = NULL
WHERE tenant.id = '01J00000000000000000000000'
  AND tenant.code = 'default'
  AND tenant.status = 'ACTIVE'
  AND org.id = '01J00000000000000000000003'
  AND org.code = 'ROOT'
  AND org.status = 'ACTIVE'
  AND position.id = '01J00000000000000000000400'
  AND position.code = 'POS-01J00000000000000000000400'
  AND position.status = 'DISABLED'
  AND NOT EXISTS (
      SELECT 1 FROM iam_bootstrap_state AS state WHERE state.tenant_id = tenant.id
  );
