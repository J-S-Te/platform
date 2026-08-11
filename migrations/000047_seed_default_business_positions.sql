-- Seed the default business positions shown by the identity-management position page.
--
-- Positions and authorization roles are separate domain objects: positions describe a user's
-- appointment in an organization, while roles grant application permissions.  The authorization
-- role catalog keeps the matching permission roles; this migration creates the organization-side
-- positions requested for appointment management.
--
-- The insert is idempotent.  A position is not recreated when the same primary key, generated
-- position code, or position name already exists under the default root organization.
INSERT INTO iam_position (
    id,
    tenant_id,
    org_unit_id,
    code,
    name,
    position_level,
    status,
    version,
    created_at,
    created_by,
    updated_at,
    updated_by
)
SELECT
    seed.id,
    tenant.id,
    org.id,
    seed.code,
    seed.name,
    NULL,
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    NULL,
    UTC_TIMESTAMP(3),
    NULL
FROM (
    SELECT '01J00000000000000000000400' AS id, 'POS-01J00000000000000000000400' AS code, '超级管理员' AS name
    UNION ALL
    SELECT '01J00000000000000000000401', 'POS-01J00000000000000000000401', '销售总监'
    UNION ALL
    SELECT '01J00000000000000000000402', 'POS-01J00000000000000000000402', '技术总监'
    UNION ALL
    SELECT '01J00000000000000000000403', 'POS-01J00000000000000000000403', '财务总监'
    UNION ALL
    SELECT '01J00000000000000000000404', 'POS-01J00000000000000000000404', '销售人员'
    UNION ALL
    SELECT '01J00000000000000000000405', 'POS-01J00000000000000000000405', '审计管理员'
) AS seed
JOIN iam_tenant AS tenant
    ON tenant.id = '01J00000000000000000000000'
JOIN iam_org_unit AS org
    ON org.id = '01J00000000000000000000003'
   AND org.tenant_id = tenant.id
WHERE NOT EXISTS (
    SELECT 1
    FROM iam_position AS existing
    WHERE existing.tenant_id = tenant.id
      AND existing.org_unit_id = org.id
      AND (
          existing.id = seed.id
          OR existing.code = seed.code
          OR existing.name = seed.name
      )
);
