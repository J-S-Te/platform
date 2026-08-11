-- Move the legacy per-user contract authorization data into the generic authorization model.
--
-- The legacy tables are intentionally retained for read-only compatibility with older platform
-- binaries. New authorization code must use authz_role_binding and authz_role_permission. The
-- deterministic SHA-256 prefixes below provide stable 26-character identifiers without adding a
-- database-specific ID generator; the generic tables do not constrain IDs to a particular ULID
-- implementation.

-- One legacy application-role row becomes one tenant-wide USER role binding. The legacy primary
-- key already prevents duplicate rows for the same tenant/application/user, while the generic
-- unique key also includes role and scope so a user can hold multiple roles after this migration.
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id, subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version, created_at, created_by, updated_at, updated_by
)
SELECT
    SUBSTRING(SHA2(CONCAT(
        _ascii'legacy-user-application-role:' COLLATE ascii_bin, legacy.tenant_id,
        _ascii':' COLLATE ascii_bin, legacy.application_id,
        _ascii':' COLLATE ascii_bin, legacy.user_id,
        _ascii':' COLLATE ascii_bin, legacy.role_id
    ), 256), 1, 26),
    legacy.tenant_id,
    legacy.application_id,
    legacy.role_id,
    'USER',
    legacy.user_id,
    'TENANT',
    '',
    NULL,
    NULL,
    'ACTIVE',
    1,
    legacy.created_at,
    legacy.created_by,
    legacy.updated_at,
    legacy.updated_by
FROM authz_user_application_role AS legacy
ON DUPLICATE KEY UPDATE id = id;

-- A direct user permission has no first-class equivalent in the existing generic schema. Preserve
-- its effective result by creating one deterministic compatibility role per user/application,
-- mapping the existing permission rows to that role, and binding that role to the user. This uses
-- only the existing authz_role, authz_role_permission, and authz_role_binding schema.
INSERT INTO authz_role (
    id, tenant_id, application_id, code, name, role_type, description, built_in, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    SUBSTRING(SHA2(CONCAT(
        _ascii'legacy-user-permissions-role:' COLLATE ascii_bin, source.tenant_id,
        _ascii':' COLLATE ascii_bin, source.application_id,
        _ascii':' COLLATE ascii_bin, source.user_id
    ), 256), 1, 26),
    source.tenant_id,
    source.application_id,
    CONCAT(_ascii'legacy-user-permissions-' COLLATE ascii_bin, source.user_id),
    CONCAT(_utf8mb4'兼容用户附加权限（', CONVERT(source.user_id USING utf8mb4), _utf8mb4'）'),
    'COMPATIBILITY',
    '由 authz_user_permission 迁移生成；仅用于保留历史用户附加权限结果。',
    0,
    'ACTIVE',
    1,
    MIN(source.created_at),
    NULL,
    MAX(source.created_at),
    NULL
FROM authz_user_permission AS source
GROUP BY source.tenant_id, source.application_id, source.user_id
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_role_permission (role_id, permission_id, effect, created_at, created_by)
SELECT
    role.id,
    source.permission_id,
    'ALLOW',
    source.created_at,
    source.created_by
FROM authz_user_permission AS source
JOIN authz_role AS role
  ON role.tenant_id = source.tenant_id
 AND role.application_id = source.application_id
 AND role.code = CONCAT(_ascii'legacy-user-permissions-' COLLATE ascii_bin, source.user_id)
ON DUPLICATE KEY UPDATE role_id = role_id;

INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id, subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version, created_at, created_by, updated_at, updated_by
)
SELECT
    SUBSTRING(SHA2(CONCAT(
        _ascii'legacy-user-permissions-binding:' COLLATE ascii_bin, source.tenant_id,
        _ascii':' COLLATE ascii_bin, source.application_id,
        _ascii':' COLLATE ascii_bin, source.user_id
    ), 256), 1, 26),
    source.tenant_id,
    source.application_id,
    role.id,
    'USER',
    source.user_id,
    'TENANT',
    '',
    NULL,
    NULL,
    'ACTIVE',
    1,
    MIN(source.created_at),
    NULL,
    MAX(source.created_at),
    NULL
FROM authz_user_permission AS source
JOIN authz_role AS role
  ON role.tenant_id = source.tenant_id
 AND role.application_id = source.application_id
 AND role.code = CONCAT(_ascii'legacy-user-permissions-' COLLATE ascii_bin, source.user_id)
GROUP BY source.tenant_id, source.application_id, source.user_id, role.id
ON DUPLICATE KEY UPDATE id = id;

-- Keep legacy tables available to older read paths, but document that they are no longer write
-- sources. Physical removal is intentionally deferred until all old binaries are retired.
ALTER TABLE authz_user_application_role
    COMMENT = 'Legacy read-only compatibility table. New writes must use authz_role_binding.';

ALTER TABLE authz_user_permission
    COMMENT = 'Legacy read-only compatibility table. Direct permissions are represented by compatibility roles in the generic model.';
