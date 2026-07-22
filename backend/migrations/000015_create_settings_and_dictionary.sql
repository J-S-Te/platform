-- Platform settings, notification preferences, and tenant-scoped business dictionaries.
-- This migration intentionally stores typed settings. SMTP, delivery dispatch, SMS, webhooks,
-- and notification templates are outside the current platform scope.

CREATE TABLE IF NOT EXISTS platform_setting (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    organization_name VARCHAR(128) NOT NULL,
    organization_alias VARCHAR(64) NOT NULL,
    timezone VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    qualification VARCHAR(500) NOT NULL DEFAULT '',
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_platform_setting_tenant (tenant_id),
    CONSTRAINT fk_platform_setting_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS notification_setting (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    inbox_enabled TINYINT(1) NOT NULL DEFAULT 1,
    email_enabled TINYINT(1) NOT NULL DEFAULT 1,
    reminder_frequency VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'DAILY',
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_setting_tenant (tenant_id),
    CONSTRAINT fk_notification_setting_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS dict_dictionary (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(100) NOT NULL,
    description VARCHAR(500) NOT NULL DEFAULT '',
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'ACTIVE',
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_dict_dictionary_tenant_code (tenant_id, code),
    KEY idx_dict_dictionary_tenant_status (tenant_id, status),
    CONSTRAINT fk_dict_dictionary_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS dict_item (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    dictionary_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    label VARCHAR(100) NOT NULL,
    item_value VARCHAR(255) NOT NULL,
    sort_order INT UNSIGNED NOT NULL DEFAULT 0,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'ACTIVE',
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_dict_item_dictionary_code (dictionary_id, code),
    KEY idx_dict_item_tenant_dictionary_status_sort (tenant_id, dictionary_id, status, sort_order),
    CONSTRAINT fk_dict_item_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_dict_item_dictionary FOREIGN KEY (dictionary_id) REFERENCES dict_dictionary (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO authz_resource (id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000120', '01J00000000000000000000000', '01J00000000000000000000001', 'platform-setting', '平台基础设置', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000121', '01J00000000000000000000000', '01J00000000000000000000001', 'notification-setting', '通知设置', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000122', '01J00000000000000000000000', '01J00000000000000000000001', 'dictionary', '业务字典', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000123', '01J00000000000000000000000', '01J00000000000000000000001', 'dictionary-item', '业务字典项', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000130', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000120', 'platform:settings:read', 'read', '查看平台基础设置', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000131', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000120', 'platform:settings:update', 'update', '更新平台基础设置', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000132', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000121', 'platform:notification-setting:read', 'read', '查看通知设置', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000133', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000121', 'platform:notification-setting:update', 'update', '更新通知设置', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000134', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000122', 'platform:dictionary:read', 'read', '查看业务字典', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000135', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000122', 'platform:dictionary:create', 'create', '创建业务字典', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000136', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000122', 'platform:dictionary:update', 'update', '更新业务字典', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000137', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000123', 'platform:dictionary-item:read', 'read', '查看业务字典项', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000138', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000123', 'platform:dictionary-item:create', 'create', '创建业务字典项', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000139', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000123', 'platform:dictionary-item:update', 'update', '更新业务字典项', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
      'platform:settings:read',
      'platform:settings:update',
      'platform:notification-setting:read',
      'platform:notification-setting:update',
      'platform:dictionary:read',
      'platform:dictionary:create',
      'platform:dictionary:update',
      'platform:dictionary-item:read',
      'platform:dictionary-item:create',
      'platform:dictionary-item:update'
  );

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 3, UTC_TIMESTAMP(3), '新增平台设置、通知设置与业务字典权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 3),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增平台设置、通知设置与业务字典权限';
