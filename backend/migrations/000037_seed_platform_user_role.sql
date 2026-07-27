-- Seed the permission-minimal role automatically assigned to newly created ordinary users.
-- This role deliberately receives no management permissions; application-specific access can be
-- granted explicitly later without turning every portal user into a platform administrator.
INSERT INTO authz_role (
    id, tenant_id, application_id, code, name, role_type, description, built_in,
    status, version, created_at, updated_at
)
VALUES (
    '01J00000000000000000000033',
    '01J00000000000000000000000',
    '01J00000000000000000000001',
    'platform-user',
    '普通用户',
    'PLATFORM',
    '新建普通用户自动绑定的基础角色；默认不包含平台管理权限。',
    1,
    'ACTIVE',
    1,
    UTC_TIMESTAMP(3),
    UTC_TIMESTAMP(3)
)
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    built_in = VALUES(built_in),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES (
    '01J00000000000000000000000',
    '01J00000000000000000000001',
    6,
    UTC_TIMESTAMP(3),
    '新增普通用户默认角色'
)
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 6),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增普通用户默认角色';
