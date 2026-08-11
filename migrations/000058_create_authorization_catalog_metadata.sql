-- Store the last authorization catalog submitted for each application.
-- The catalog itself remains owned by the subsystem; this table records only the
-- platform-side synchronization state needed for auditability and idempotent syncs.
CREATE TABLE IF NOT EXISTS authz_authorization_catalog (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    catalog_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    catalog_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_identifier VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    sync_status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_synced_at DATETIME(3) NULL,
    last_synced_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_message VARCHAR(2000) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (tenant_id, application_id),
    KEY idx_authz_catalog_sync_status (tenant_id, sync_status),
    KEY idx_authz_catalog_version (application_id, catalog_version),
    CONSTRAINT fk_authz_catalog_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_catalog_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Latest platform-side authorization catalog synchronization state; subsystem remains the catalog owner.';
