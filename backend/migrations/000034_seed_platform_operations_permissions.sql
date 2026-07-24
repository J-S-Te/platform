INSERT INTO authz_resource (id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000192', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-application-login-target', '应用登录目标', 'PLATFORM', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000193', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-notification', '站内信通知', 'PLATFORM', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000194', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-observability', '可观测性', 'PLATFORM', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000195', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-audit-operations', '审计运营', 'PLATFORM', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000196', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-file-task', '文件与异步任务', 'PLATFORM', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000192', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000192', 'platform:application-login-target:read', 'read', '查看应用登录目标', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000193', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000192', 'platform:application-login-target:create', 'create', '创建应用登录目标', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000194', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000192', 'platform:application-login-target:update', 'update', '更新应用登录目标', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000195', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000193', 'platform:notification:template:read', 'template-read', '查看通知模板', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000196', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000193', 'platform:notification:template:create', 'template-create', '创建通知模板', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000197', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000193', 'platform:notification:template:update', 'template-update', '更新通知模板', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000198', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000193', 'platform:notification:operate', 'operate', '创建通知并管理投递', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000199', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000194', 'platform:observability:log:view', 'log-view', '查看运行日志', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000200', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000194', 'platform:observability:trace:view', 'trace-view', '查看调用链', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000201', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000194', 'platform:observability:metric:view', 'metric-view', '查看运行指标', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000202', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000194', 'platform:observability:alert:manage', 'alert-manage', '管理告警规则', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000203', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000194', 'platform:observability:alert:execute', 'alert-execute', '执行告警规则', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000204', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000195', 'platform:audit:ingestion-receipt:view', 'receipt-view', '查看审计接收回执', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000205', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000195', 'platform:audit:dead-letter:view', 'dead-letter-view', '查看审计死信', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000206', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000195', 'platform:audit:dead-letter:replay', 'dead-letter-replay', '重放审计死信', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000207', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000195', 'platform:audit:retention:manage', 'retention-manage', '管理审计归档与清理', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000208', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:file:upload', 'file-upload', '上传本地文件', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000209', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:file:download', 'file-download', '下载授权文件', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000210', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:file:cleanup', 'file-cleanup', '清理过期文件', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000211', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:async-job:create', 'job-create', '创建异步任务', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000212', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:async-job:read', 'job-read', '查看异步任务', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000213', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:async-job:cancel', 'job-cancel', '取消异步任务', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000214', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:async-job:retry', 'job-retry', '重试异步任务', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000215', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000196', 'platform:async-job:rerun', 'job-rerun', '重新执行异步任务', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT '01J00000000000000000000030', id, 'ALLOW', UTC_TIMESTAMP(3) FROM authz_permission
WHERE tenant_id = '01J00000000000000000000000' AND application_id = '01J00000000000000000000001' AND code IN (
    'platform:application-login-target:read',
    'platform:application-login-target:create',
    'platform:application-login-target:update',
    'platform:notification:template:read',
    'platform:notification:template:create',
    'platform:notification:template:update',
    'platform:notification:operate',
    'platform:observability:log:view',
    'platform:observability:trace:view',
    'platform:observability:metric:view',
    'platform:observability:alert:manage',
    'platform:observability:alert:execute',
    'platform:audit:ingestion-receipt:view',
    'platform:audit:dead-letter:view',
    'platform:audit:dead-letter:replay',
    'platform:audit:retention:manage',
    'platform:file:upload',
    'platform:file:download',
    'platform:file:cleanup',
    'platform:async-job:create',
    'platform:async-job:read',
    'platform:async-job:cancel',
    'platform:async-job:retry',
    'platform:async-job:rerun'
);

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT '01J00000000000000000000032', id, 'ALLOW', UTC_TIMESTAMP(3) FROM authz_permission
WHERE application_id = '01J00000000000000000000001' AND code IN (
    'platform:audit:ingestion-receipt:view', 'platform:audit:dead-letter:view',
    'platform:audit:dead-letter:replay', 'platform:audit:retention:manage'
);

UPDATE authz_policy_revision SET revision = revision + 1, changed_at = UTC_TIMESTAMP(3), change_reason = 'add platform operations permissions'
WHERE tenant_id = '01J00000000000000000000000' AND application_id = '01J00000000000000000000001';
