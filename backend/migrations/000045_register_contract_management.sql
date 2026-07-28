-- Register contract_management as a first-class platform subsystem. Its environment record is
-- consumed by scripts/portal-gateway.sh and the permissions are returned by /api/v1/auth/me.

INSERT INTO platform_application (
    id, tenant_id, code, name, application_type, homepage_url, description,
    status, version, created_at, updated_at
)
VALUES (
    '01J00000000000000000000300',
    '01J00000000000000000000000',
    'contract_management',
    '合同管理系统',
    'web',
    'http://localhost:8081/contract/',
    '合同状态机、合同审批和审批规则子系统。',
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
)
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO platform_application_environment (
    id, tenant_id, application_id, environment, base_url, upstream_url, path_prefix,
    issuer_alias, metadata, status, version, created_at, updated_at
)
SELECT
    '01J00000000000000000000301',
    '01J00000000000000000000000',
    application.id,
    'dev',
    'http://localhost:8081',
    'http://contract-api:8081',
    '/contract',
    NULL,
    JSON_OBJECT('health_path', '/healthz', 'api_base_path', '/api/v1'),
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
FROM platform_application AS application
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
ON DUPLICATE KEY UPDATE
    base_url = VALUES(base_url),
    upstream_url = VALUES(upstream_url),
    path_prefix = VALUES(path_prefix),
    metadata = VALUES(metadata),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO platform_application_login_target (
    id, tenant_id, application_id, environment_id, target_code, name, target_uri,
    status, version, created_at, updated_at
)
SELECT
    '01J00000000000000000000302',
    application.tenant_id,
    application.id,
    environment.id,
    'home',
    '合同管理系统首页',
    '/contract/',
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
FROM platform_application AS application
JOIN platform_application_environment AS environment
  ON environment.application_id = application.id
 AND environment.tenant_id = application.tenant_id
 AND environment.environment = 'dev'
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    target_uri = VALUES(target_uri),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO authz_resource (
    id, tenant_id, application_id, code, name, resource_type, attribute_schema,
    status, version, created_at, updated_at
)
VALUES
    ('01J00000000000000000000302', '01J00000000000000000000000', '01J00000000000000000000001', 'contract-management-contract', '合同管理', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000303', '01J00000000000000000000000', '01J00000000000000000000001', 'contract-management-approval', '合同审批', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000304', '01J00000000000000000000000', '01J00000000000000000000001', 'contract-management-approval-rule', '合同审批规则', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO authz_permission (
    id, tenant_id, application_id, resource_id, code, action, name, description,
    risk_level, status, version, created_at, updated_at
)
VALUES
    ('01J00000000000000000000310', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000302', 'platform:contract:create', 'create', '创建与提交合同', '创建本人合同并提交审批。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000311', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000302', 'platform:contract:read', 'read', '查看合同', '查看本人合同。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000312', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000302', 'platform:contract:edit', 'edit', '变更合同状态', '执行合同状态变更。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000313', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000302', 'platform:contract:manage', 'manage', '管理全部合同', '跨负责人管理合同。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000314', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000303', 'platform:approval:process', 'process', '处理合同审批', '处理分配给当前用户的合同审批任务。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000315', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000303', 'platform:approval:view', 'view', '查看合同审批', '查看有权访问的合同审批。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000316', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000303', 'platform:approval:manage', 'manage', '管理合同审批', '管理和催办全部合同审批。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000317', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000304', 'platform:approval_rule:manage', 'manage', '管理合同审批规则', '创建、修改和删除合同审批规则。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    risk_level = VALUES(risk_level),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT '01J00000000000000000000030', permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_permission AS permission
WHERE permission.tenant_id = '01J00000000000000000000000'
  AND permission.application_id = '01J00000000000000000000001'
  AND permission.code IN (
      'platform:contract:create',
      'platform:contract:read',
      'platform:contract:edit',
      'platform:contract:manage',
      'platform:approval:process',
      'platform:approval:view',
      'platform:approval:manage',
      'platform:approval_rule:manage'
  );

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT '01J00000000000000000000033', permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_permission AS permission
WHERE permission.tenant_id = '01J00000000000000000000000'
  AND permission.application_id = '01J00000000000000000000001'
  AND permission.code IN (
      'platform:contract:create',
      'platform:contract:read',
      'platform:approval:process',
      'platform:approval:view'
  );

UPDATE authz_policy_revision
SET revision = revision + 1,
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '注册合同管理子系统权限'
WHERE tenant_id = '01J00000000000000000000000'
  AND application_id = '01J00000000000000000000001';
