-- Permissions for normal local-account provisioning and administrator password lifecycle.
-- The reset endpoint returns a generated password only in the immediate HTTP response; no password
-- material is stored in authorization data, audit records, or migration seed data.

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000140', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000011', 'platform:account:create', 'create', '创建本地账号', '为已有用户创建 HUMAN/LOCAL 账号及初始密码凭据。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000141', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000011', 'platform:account:password-initialize', 'password-initialize', '初始化账号密码', '仅为尚未拥有本地密码凭据的 HUMAN/LOCAL 账号初始化密码。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000142', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000011', 'platform:account:password-reset', 'password-reset', '管理员重置账号密码', '生成强密码并仅在当前响应中返回，供管理员线下交付。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
      'platform:account:create',
      'platform:account:password-initialize',
      'platform:account:password-reset'
  );

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 4, UTC_TIMESTAMP(3), '新增本地账号和密码生命周期权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 4),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增本地账号和密码生命周期权限';
