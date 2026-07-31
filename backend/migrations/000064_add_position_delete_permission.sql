-- Adds the dedicated high-risk permission used by the versioned logical-delete position API.
-- Existing migrations remain immutable because the migration runner verifies applied checksums.

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000177', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000013', 'platform:position:delete', 'delete', '删除岗位', '逻辑删除岗位，同步停用关联任职关系，并使岗位继承授权失效。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    risk_level = VALUES(risk_level),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT role.id, permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_role AS role
JOIN authz_permission AS permission
    ON permission.tenant_id = role.tenant_id
    AND permission.application_id = role.application_id
WHERE role.tenant_id = '01J00000000000000000000000'
  AND role.application_id = '01J00000000000000000000001'
  AND role.code IN ('platform-super-admin', 'platform-security-admin')
  AND permission.code = 'platform:position:delete';

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 15, UTC_TIMESTAMP(3), '新增岗位逻辑删除权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 15),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增岗位逻辑删除权限';
