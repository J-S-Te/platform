-- 文件资源绑定必须使用独立权限，不能复用上传或下载权限扩大业务资源修改能力。
INSERT INTO authz_permission (
    id, tenant_id, application_id, resource_id, code, action, name,
    description, risk_level, status, version, created_at, updated_at
)
SELECT
    '01J00000000000000000000218', tenant.id, '01J00000000000000000000001',
    '01J00000000000000000000196', 'platform:file:bind', 'file-bind', '维护文件资源绑定',
    '将文件绑定或解绑到业务资源', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)
FROM iam_tenant AS tenant
WHERE tenant.id = '01J00000000000000000000000' AND tenant.status = 'ACTIVE'
ON DUPLICATE KEY UPDATE name = VALUES(name), description = VALUES(description), risk_level = VALUES(risk_level), status = 'ACTIVE', updated_at = UTC_TIMESTAMP(3);

-- 平台超级管理员沿用既有平台角色，新增权限显式加入该角色；普通角色需要管理员按需授予。
INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT role.id, permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_role AS role
JOIN authz_permission AS permission
  ON permission.tenant_id = role.tenant_id
 AND permission.application_id = role.application_id
 AND permission.code = 'platform:file:bind'
WHERE role.application_id = '01J00000000000000000000001'
  AND role.code = 'super_admin'
  AND role.status = 'ACTIVE';
