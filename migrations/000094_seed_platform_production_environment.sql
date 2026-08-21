-- 生产 API/Worker 使用 AUDIT_ENVIRONMENT_CODE=prod；确保平台自身的审计来源
-- 在生产数据库中有对应环境，避免审计写入因找不到资源而被丢弃。
INSERT INTO platform_application_environment (
    id,
    tenant_id,
    application_id,
    environment,
    base_url,
    issuer_alias,
    metadata,
    status,
    version,
    created_at,
    updated_at
)
SELECT
    '01K3QH7F0P9A4D6B8C2E1F3G5H',
    application.tenant_id,
    application.id,
    'prod',
    NULL,
    'platform',
    NULL,
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
FROM platform_application AS application
WHERE application.code = 'platform'
  AND NOT EXISTS (
      SELECT 1
      FROM platform_application_environment AS existing
      WHERE existing.application_id = application.id
        AND existing.environment = 'prod'
  );
