-- Persistent inheritance from one platform role to application-owned roles.
-- Position templates keep only the source platform role; generated bindings retain
-- their normal TEMPLATE provenance and are expanded from this mapping at sync time.
CREATE TABLE IF NOT EXISTS authz_role_inheritance_mapping (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_role_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_role_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'TENANT',
    scope_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'ACTIVE',
    valid_from DATETIME(3) NULL,
    valid_until DATETIME(3) NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_role_inheritance_mapping (tenant_id, source_role_id, target_application_id, target_role_id, scope_type, scope_id),
    KEY idx_role_inheritance_source (tenant_id, source_role_id, status),
    CONSTRAINT fk_role_inheritance_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_inheritance_source_role FOREIGN KEY (tenant_id, source_application_id, source_role_id)
        REFERENCES authz_role (tenant_id, application_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_inheritance_target_role FOREIGN KEY (tenant_id, target_application_id, target_role_id)
        REFERENCES authz_role (tenant_id, application_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Platform role to subsystem role inheritance mappings.';
