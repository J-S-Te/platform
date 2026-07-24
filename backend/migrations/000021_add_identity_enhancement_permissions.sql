-- 身份增强能力的控制台权限：外部身份提供商、外部身份绑定、
-- OAuth 客户端独立登出回调地址与公钥注册。
-- 用户本人 MFA 与授权同意操作通过已认证会话的“本人边界”保护，
-- 不授予管理员代办权限，避免越权查看或处置第二认证因子。

INSERT INTO authz_resource (id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000180', '01J00000000000000000000000', '01J00000000000000000000001', 'identity-provider', '外部身份提供商', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000181', '01J00000000000000000000000', '01J00000000000000000000001', 'identity-binding', '外部身份绑定', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000182', '01J00000000000000000000000', '01J00000000000000000000001', 'oauth-client-post-logout-redirect-uri', '登出后回调地址', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000183', '01J00000000000000000000000', '01J00000000000000000000001', 'oauth-client-jwk', 'OAuth 客户端公钥', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000184', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000180', 'platform:identity-provider:read', 'read', '查看外部身份提供商', '查询受控外部身份提供商配置的非敏感元数据。', 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000185', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000180', 'platform:identity-provider:create', 'create', '创建外部身份提供商', '注册外部身份提供商的本地受控配置。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000186', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000180', 'platform:identity-provider:update', 'update', '更新外部身份提供商', '更新外部身份提供商显示信息或启停状态。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000187', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000181', 'platform:identity-binding:read', 'read', '查看外部身份绑定', '查询用户与外部身份主体的绑定元数据，不返回外部主体明文。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000188', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000181', 'platform:identity-binding:create', 'create', '创建外部身份绑定', '将已校验的外部身份主体绑定到平台用户。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000189', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000181', 'platform:identity-binding:delete', 'delete', '解绑外部身份', '解除用户与外部身份主体之间的绑定。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000190', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000182', 'platform:oauth-client:post-logout-redirect-uri-update', 'post-logout-redirect-uri-update', '更新登出后回调地址', '替换 OAuth 客户端独立登记的登出后回调地址集合。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000191', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000183', 'platform:oauth-client:jwk-update', 'jwk-update', '更新 OAuth 客户端公钥', '替换 OAuth 客户端用于 private_key_jwt 与 JAR 的公开 JWK 集合。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
      'platform:identity-provider:read',
      'platform:identity-provider:create',
      'platform:identity-provider:update',
      'platform:identity-binding:read',
      'platform:identity-binding:create',
      'platform:identity-binding:delete',
      'platform:oauth-client:post-logout-redirect-uri-update',
      'platform:oauth-client:jwk-update'
  );

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 6, UTC_TIMESTAMP(3), '新增身份增强与 OAuth 客户端公钥管理权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 6),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增身份增强与 OAuth 客户端公钥管理权限';
