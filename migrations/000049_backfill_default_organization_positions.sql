-- Organizations created before the platform began provisioning standard appointment
-- positions (including the existing "合同管理" organization) have no selectable
-- positions. Backfill the standard catalog for every active organization, while
-- preserving any position that an administrator has already created or disabled.
--
-- The IDs are deterministic, ULID-shaped opaque identifiers derived from the
-- organization ID and template key, making this safe to apply once per schema and
-- preventing the same template position from being inserted twice for one org.
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
    CONCAT('01', UPPER(SUBSTRING(SHA2(CONCAT(CONVERT(org.id USING utf8mb4), _utf8mb4':', seed.template_key), 256), 1, 24))),
    org.tenant_id,
    org.id,
    CONCAT(_utf8mb4'POS-DEFAULT-', seed.template_key),
    seed.name,
    NULL,
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    NULL,
    UTC_TIMESTAMP(3),
    NULL
FROM iam_org_unit AS org
CROSS JOIN (
    SELECT 'SUPER_ADMIN' AS template_key, '超级管理员' AS name
    UNION ALL SELECT 'SALES_DIRECTOR', '销售总监'
    UNION ALL SELECT 'TECHNICAL_DIRECTOR', '技术总监'
    UNION ALL SELECT 'FINANCE_DIRECTOR', '财务总监'
    UNION ALL SELECT 'SALES_REPRESENTATIVE', '销售人员'
    UNION ALL SELECT 'AUDIT_ADMINISTRATOR', '审计管理员'
) AS seed
WHERE org.status = 'ACTIVE'
  AND NOT EXISTS (
      SELECT 1
      FROM iam_position AS existing
      WHERE existing.tenant_id = org.tenant_id
        AND existing.org_unit_id = org.id
        AND (CONVERT(existing.code USING utf8mb4) = CONCAT(_utf8mb4'POS-DEFAULT-', seed.template_key) OR existing.name = seed.name)
  );
