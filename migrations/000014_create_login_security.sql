-- Login security and risk-control storage.
-- Passwords, token values, and other authentication secrets must never be written to these tables.

CREATE TABLE IF NOT EXISTS sec_login_policy (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    max_failed_attempts INT UNSIGNED NOT NULL,
    lockout_duration_seconds INT UNSIGNED NOT NULL,
    failure_reset_window_seconds INT UNSIGNED NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (tenant_id),
    CONSTRAINT fk_sec_login_policy_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sec_login_attempt (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    occurred_at DATETIME(3) NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    username_snapshot VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ip_address VARBINARY(16) NULL,
    user_agent VARCHAR(1000) NULL,
    result VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    failure_reason VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    risk_score INT UNSIGNED NOT NULL DEFAULT 0,
    cleared_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    KEY idx_sec_login_attempt_account_active (tenant_id, account_id, result, cleared_at, occurred_at),
    KEY idx_sec_login_attempt_tenant_occurred (tenant_id, occurred_at),
    CONSTRAINT fk_sec_login_attempt_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_sec_login_attempt_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sec_risk_event (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    event_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    risk_level VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    risk_score INT UNSIGNED NOT NULL DEFAULT 0,
    source_ip VARBINARY(16) NULL,
    detection_rule VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    occurred_at DATETIME(3) NOT NULL,
    resolved_at DATETIME(3) NULL,
    resolved_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    resolution_comment VARCHAR(500) NULL,
    metadata JSON NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_sec_risk_event_tenant_status_occurred (tenant_id, status, occurred_at),
    KEY idx_sec_risk_event_tenant_account_occurred (tenant_id, account_id, occurred_at),
    CONSTRAINT fk_sec_risk_event_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_sec_risk_event_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT,
    CONSTRAINT fk_sec_risk_event_resolved_by FOREIGN KEY (resolved_by) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO authz_resource (id, tenant_id, application_id, code, name, resource_type, attribute_schema, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000100', '01J00000000000000000000000', '01J00000000000000000000001', 'security-policy', '登录安全策略', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000101', '01J00000000000000000000000', '01J00000000000000000000001', 'locked-account', '锁定账号', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000102', '01J00000000000000000000000', '01J00000000000000000000001', 'risk-event', '安全风险事件', 'API', NULL, 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000110', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000100', 'platform:security-policy:read', 'read', '查看登录安全策略', NULL, 'LOW', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000111', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000100', 'platform:security-policy:update', 'update', '更新登录安全策略', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000112', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000101', 'platform:locked-account:read', 'read', '查看锁定账号', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000113', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000101', 'platform:locked-account:unlock', 'unlock', '解除账号锁定', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000114', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000102', 'platform:risk-event:read', 'read', '查看安全风险事件', NULL, 'MEDIUM', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000115', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000102', 'platform:risk-event:resolve', 'resolve', '处置安全风险事件', NULL, 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
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
      'platform:security-policy:read',
      'platform:security-policy:update',
      'platform:locked-account:read',
      'platform:locked-account:unlock',
      'platform:risk-event:read',
      'platform:risk-event:resolve'
  );

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 2, UTC_TIMESTAMP(3), '新增登录安全与风险控制权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 2),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增登录安全与风险控制权限';
