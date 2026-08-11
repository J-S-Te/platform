-- Adds the organization maintenance permissions required by the management console's
-- versioned edit and logical-delete operations. This is deliberately a new migration: applied
-- migration files must remain immutable so checksum verification remains valid.

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000175', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000012', 'platform:organization:update', 'update', '编辑组织', '编辑组织名称、上级组织和排序；后端会校验层级并原子更新子树路径。', 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000176', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000012', 'platform:organization:delete', 'delete', '删除组织', '逻辑删除组织子树，并同步停用关联岗位和任职关系。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
  AND permission.code IN ('platform:organization:update', 'platform:organization:delete');

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 14, UTC_TIMESTAMP(3), '新增组织编辑与逻辑删除权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 14),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增组织编辑与逻辑删除权限';
