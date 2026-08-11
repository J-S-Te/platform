-- Store application-owned authorization policy constraints separately from roles and permissions.
-- A value of 0 for max_effective_roles means unlimited effective roles.  Policy values are
-- supplied only by a verified application catalog synchronization and are retained with the
-- catalog provenance/version that set them; the platform does not infer policy from an
-- application code, role name, or permission code.
CREATE TABLE IF NOT EXISTS authz_application_authorization_policy (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    max_effective_roles SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    source_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_identifier VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    catalog_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    catalog_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_synced_at DATETIME(3) NULL,
    last_synced_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (tenant_id, application_id),
    KEY idx_authz_application_policy_limit (tenant_id, max_effective_roles),
    CONSTRAINT fk_authz_application_policy_tenant
        FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_authz_application_policy_application
        FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Application-owned authorization catalog policy constraints; zero max_effective_roles means unlimited.';
