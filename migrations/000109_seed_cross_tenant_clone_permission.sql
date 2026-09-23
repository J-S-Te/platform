-- 跨租户授权目录克隆是平台级特权（SEC-D1）：向调用方租户之外的目标租户整树写入
-- 应用/资源/权限/角色，必须由专门权限把关。此前 HTTP 路径只校验登录与租户内权限，
-- 任何租户管理员拿到他人租户 ULID 即可注入目录并以 (tenant_id,code) 唯一约束阻塞受害者接入。
-- 本迁移只落权限「定义」，故意不授予任何角色（fail-closed）：需要跨租户克隆能力的
-- 平台运维角色由管理员在授权控制台显式绑定后才生效；普通租户角色默认永远拿不到。

INSERT INTO authz_resource (
    id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at
)
VALUES
    ('01J00000000000000000000216', '01J00000000000000000000000', '01J00000000000000000000001', 'tenant-authorization-catalog-clone', '租户授权目录克隆', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (
    id, tenant_id, application_id, resource_id, code, action, name,
    description, risk_level, status, version, created_at, updated_at
)
VALUES
    ('01J00000000000000000000217', '01J00000000000000000000000', '01J00000000000000000000001',
     '01J00000000000000000000216', 'platform:tenant:authorization-catalog-clone',
     'authorization-catalog-clone', '跨租户授权目录克隆',
     '把当前租户的静态授权目录克隆到其他租户；仅授予平台级运维角色，普通租户管理员不得持有。',
     'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE name = VALUES(name), description = VALUES(description), risk_level = VALUES(risk_level), status = 'ACTIVE', updated_at = UTC_TIMESTAMP(3);
