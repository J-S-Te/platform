-- 审计事件查询与导出仍由基础审计模块提供；移除不再开放的“审计运营”管理模块。
-- 审计接收回执、死信和保留任务的数据表继续保留，供后台 Worker 做可靠性处理。
DELETE role_permission
FROM authz_role_permission AS role_permission
JOIN authz_permission AS permission ON permission.id = role_permission.permission_id
WHERE permission.tenant_id = '01J00000000000000000000000'
  AND permission.application_id = '01J00000000000000000000001'
  AND permission.code IN (
      'platform:audit:ingestion-receipt:view',
      'platform:audit:dead-letter:view',
      'platform:audit:dead-letter:replay',
      'platform:audit:retention:manage'
  );

DELETE FROM authz_permission
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001'
  AND code IN (
      'platform:audit:ingestion-receipt:view',
      'platform:audit:dead-letter:view',
      'platform:audit:dead-letter:replay',
      'platform:audit:retention:manage'
  );

DELETE FROM authz_resource
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001'
  AND code = 'platform-audit-operations';

UPDATE authz_policy_revision
SET revision = revision + 1,
    changed_at = UTC_TIMESTAMP(3),
    change_reason = 'remove audit operations module'
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001';
