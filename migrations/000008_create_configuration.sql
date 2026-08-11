CREATE TABLE IF NOT EXISTS cfg_namespace (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    description VARCHAR(1000) NULL,
    current_release_no BIGINT UNSIGNED NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_cfg_namespace (tenant_id, application_id, environment_id, name),
    CONSTRAINT fk_cfg_namespace_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_cfg_namespace_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_cfg_namespace_environment FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS cfg_item (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    namespace_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    config_key VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    value_type VARCHAR(32) NOT NULL,
    value_text TEXT NULL,
    value_json JSON NULL,
    secret_ref VARCHAR(512) NULL,
    schema_json JSON NULL,
    description VARCHAR(1000) NULL,
    `sensitive` TINYINT(1) NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_cfg_item_key (namespace_id, config_key),
    CONSTRAINT fk_cfg_item_namespace FOREIGN KEY (namespace_id) REFERENCES cfg_namespace (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS cfg_release (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    namespace_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    release_no BIGINT UNSIGNED NOT NULL,
    release_status VARCHAR(32) NOT NULL,
    item_count INT UNSIGNED NOT NULL,
    checksum BINARY(32) NOT NULL,
    change_summary VARCHAR(1000) NULL,
    released_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    released_at DATETIME(3) NULL,
    rollback_from_release_no BIGINT UNSIGNED NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_cfg_release_no (namespace_id, release_no),
    CONSTRAINT fk_cfg_release_namespace FOREIGN KEY (namespace_id) REFERENCES cfg_namespace (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS cfg_release_item (
    release_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    config_key VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    value_type VARCHAR(32) NOT NULL,
    value_text TEXT NULL,
    value_json JSON NULL,
    secret_ref VARCHAR(512) NULL,
    schema_json JSON NULL,
    `sensitive` TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (release_id, config_key),
    CONSTRAINT fk_cfg_release_item_release FOREIGN KEY (release_id) REFERENCES cfg_release (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
