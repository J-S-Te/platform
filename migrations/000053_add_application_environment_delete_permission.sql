-- Grants controlled environment offboarding to platform administrators. The implementation
-- requires exact application/environment confirmation and preserves configuration and audit
-- evidence by refusing deletion while either still exists.

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000174', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000151', 'platform:application-environment:delete', 'delete', '删除应用环境', '撤销非开发环境接入，并清理该环境派生的登录目标和 OAuth 客户端配置。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
  AND permission.code = 'platform:application-environment:delete';

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 13, UTC_TIMESTAMP(3), '新增受控撤销应用环境接入权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 13),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增受控撤销应用环境接入权限';
