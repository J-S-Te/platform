CREATE TABLE IF NOT EXISTS authz_resource (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    resource_type VARCHAR(32) NOT NULL,
    attribute_schema JSON NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_resource_code (application_id, code),
    KEY idx_resource_tenant (tenant_id, application_id, status),
    CONSTRAINT fk_authz_resource_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_resource_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS authz_permission (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(1000) NULL,
    risk_level VARCHAR(16) NOT NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_permission_code (application_id, code),
    UNIQUE KEY uk_resource_action (resource_id, action),
    KEY idx_permission_tenant (tenant_id, application_id, status),
    CONSTRAINT fk_authz_permission_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_permission_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_permission_resource FOREIGN KEY (resource_id) REFERENCES authz_resource (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS authz_role (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    role_type VARCHAR(32) NOT NULL,
    description VARCHAR(1000) NULL,
    built_in TINYINT(1) NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_role_code (tenant_id, application_id, code),
    KEY idx_role_application (tenant_id, application_id, status),
    CONSTRAINT fk_authz_role_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_role_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS authz_role_permission (
    role_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    permission_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    effect VARCHAR(16) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (role_id, permission_id),
    CONSTRAINT fk_role_permission_role FOREIGN KEY (role_id) REFERENCES authz_role (id) ON DELETE CASCADE,
    CONSTRAINT fk_role_permission_permission FOREIGN KEY (permission_id) REFERENCES authz_permission (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS authz_role_binding (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    role_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_type VARCHAR(32) NOT NULL,
    subject_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_type VARCHAR(32) NOT NULL,
    scope_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    valid_from DATETIME(3) NULL,
    valid_until DATETIME(3) NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_role_binding (tenant_id, application_id, role_id, subject_type, subject_id, scope_type, scope_id),
    KEY idx_role_binding_subject (tenant_id, application_id, subject_type, subject_id, status),
    CONSTRAINT fk_role_binding_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_binding_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_binding_role FOREIGN KEY (role_id) REFERENCES authz_role (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS authz_policy_revision (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
    changed_at DATETIME(3) NOT NULL,
    change_reason VARCHAR(255) NOT NULL,
    PRIMARY KEY (tenant_id, application_id),
    CONSTRAINT fk_policy_revision_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_policy_revision_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
