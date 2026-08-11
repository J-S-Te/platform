-- Add the explicit permission for logical user deletion. The API retains identity rows for
-- referential integrity, disables all login paths, and revokes active sessions atomically.
INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES (
    '01J00000000000000000000160',
    '01J00000000000000000000000',
    '01J00000000000000000000001',
    '01J00000000000000000000010',
    'platform:user:delete',
    'delete',
    '删除用户',
    '逻辑删除用户，停用关联账号和任职关系，并撤销现有登录会话。',
    'HIGH',
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
)
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
  AND permission.code = 'platform:user:delete';

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 5, UTC_TIMESTAMP(3), '新增用户逻辑删除权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 5),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增用户逻辑删除权限';
