-- Console permissions for application registration and OAuth client credential management.
-- OAuth client secrets are generated at request time and only their bcrypt digest/fingerprint
-- metadata is persisted; this migration contains no credential material.

INSERT INTO authz_resource (id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000150', '01J00000000000000000000000', '01J00000000000000000000001', 'application', '接入应用', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000151', '01J00000000000000000000000', '01J00000000000000000000001', 'application-environment', '应用环境', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000152', '01J00000000000000000000000', '01J00000000000000000000001', 'oauth-client', 'OAuth 客户端', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000153', '01J00000000000000000000000', '01J00000000000000000000001', 'oauth-client-credential', 'OAuth 客户端密钥', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000160', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000150', 'platform:application:read', 'read', '查看接入应用', '查询接入应用及其基本注册信息。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000161', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000150', 'platform:application:create', 'create', '创建接入应用', '注册新的接入应用。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000162', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000150', 'platform:application:update', 'update', '更新接入应用', '更新接入应用资料或其生命周期状态。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000163', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000151', 'platform:application-environment:read', 'read', '查看应用环境', '查询接入应用的环境配置。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000164', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000151', 'platform:application-environment:create', 'create', '创建应用环境', '为接入应用创建环境边界。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000165', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000151', 'platform:application-environment:update', 'update', '更新应用环境', '更新应用环境资料或其启用状态。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000166', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000152', 'platform:oauth-client:read', 'read', '查看 OAuth 客户端', '查询 OAuth 客户端、授权范围和回调地址的非敏感信息。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000167', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000152', 'platform:oauth-client:create', 'create', '创建 OAuth 客户端', '创建 OAuth 客户端及其初始受控配置。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000168', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000152', 'platform:oauth-client:scope-update', 'scope-update', '更新 OAuth 客户端范围', '替换 OAuth 客户端允许申请的 scope 集合。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000169', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000152', 'platform:oauth-client:redirect-uri-update', 'redirect-uri-update', '更新 OAuth 回调地址', '替换 OAuth 客户端允许使用的回调地址。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000170', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000152', 'platform:oauth-client:disable', 'disable', '禁用 OAuth 客户端', '禁用 OAuth 客户端并撤销其仍有效的密钥。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000171', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000153', 'platform:oauth-client-credential:create', 'create', '创建 OAuth 客户端密钥', '创建额外客户端密钥，明文仅在当前响应中返回。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000172', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000153', 'platform:oauth-client-credential:rotate', 'rotate', '轮换 OAuth 客户端密钥', '生成新密钥，并在受限重叠期后撤销旧密钥。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000173', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000153', 'platform:oauth-client-credential:disable', 'disable', '禁用 OAuth 客户端密钥', '撤销指定客户端密钥，不暴露密钥摘要或明文。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT role.id, permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_role AS role
JOIN authz_permission AS permission
    ON permission.tenant_id = role.tenant_id
    AND permission.application_id = role.application_id
WHERE role.tenant_id = '01J00000000000000000000000'
  AND role.application_id = '01J00000000000000000000001'
  AND role.code IN ('platform-super-admin', 'platform-security-admin')
  AND permission.code IN (
      'platform:application:read',
      'platform:application:create',
      'platform:application:update',
      'platform:application-environment:read',
      'platform:application-environment:create',
      'platform:application-environment:update',
      'platform:oauth-client:read',
      'platform:oauth-client:create',
      'platform:oauth-client:scope-update',
      'platform:oauth-client:redirect-uri-update',
      'platform:oauth-client:disable',
      'platform:oauth-client-credential:create',
      'platform:oauth-client-credential:rotate',
      'platform:oauth-client-credential:disable'
  );

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 5, UTC_TIMESTAMP(3), '新增接入应用和 OAuth 客户端受控管理权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 5),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增接入应用和 OAuth 客户端受控管理权限';
