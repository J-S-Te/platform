-- 移除不再开放的风险事件、运行可观测和可观测性管理模块。
-- 登录失败记录、账号锁定、基础结构化日志和健康检查仍继续保留。
DELETE role_permission
FROM authz_role_permission AS role_permission
JOIN authz_permission AS permission ON permission.id = role_permission.permission_id
WHERE permission.tenant_id = '01J00000000000000000000000'
  AND permission.application_id = '01J00000000000000000000001'
  AND permission.code IN (
      'platform:risk-event:read',
      'platform:risk-event:resolve',
      'platform:observability:log:view',
      'platform:observability:trace:view',
      'platform:observability:metric:view',
      'platform:observability:alert:manage',
      'platform:observability:alert:execute'
  );

DELETE FROM authz_permission
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001'
  AND code IN (
      'platform:risk-event:read',
      'platform:risk-event:resolve',
      'platform:observability:log:view',
      'platform:observability:trace:view',
      'platform:observability:metric:view',
      'platform:observability:alert:manage',
      'platform:observability:alert:execute'
  );

DELETE FROM authz_resource
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001'
  AND code IN ('risk-event', 'platform-observability');

DROP TABLE IF EXISTS obs_alert_evaluation;
DROP TABLE IF EXISTS obs_alert_rule;
DROP TABLE IF EXISTS sec_risk_event;

ALTER TABLE sec_login_attempt
    DROP COLUMN risk_score;

UPDATE authz_policy_revision
SET revision = revision + 1,
    changed_at = UTC_TIMESTAMP(3),
    change_reason = 'remove risk event and observability modules'
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001';
