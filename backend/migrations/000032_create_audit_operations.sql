CREATE TABLE IF NOT EXISTS audit_retention_task (
    task_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    requested_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    worker_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    cutoff_at DATETIME(3) NOT NULL,
    candidate_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
    processed_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
    failure_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    failure_message VARCHAR(1000) NULL,
    created_at DATETIME(3) NOT NULL,
    started_at DATETIME(3) NULL,
    completed_at DATETIME(3) NULL,
    PRIMARY KEY (task_id),
    KEY idx_audit_retention_task_tenant_status_created (tenant_id, status, created_at),
    KEY idx_audit_retention_task_claim (status, started_at, created_at),
    KEY idx_audit_retention_task_application_cutoff (application_id, cutoff_at),
    CONSTRAINT fk_audit_retention_task_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_audit_retention_task_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_audit_retention_task_requested_by FOREIGN KEY (requested_by) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS audit_archive (
    archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    storage_relative_path VARCHAR(1000) NOT NULL,
    media_type VARCHAR(128) NOT NULL,
    sha256 BINARY(32) NOT NULL,
    event_count BIGINT UNSIGNED NOT NULL,
    occurred_from DATETIME(3) NOT NULL,
    occurred_to DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (archive_id),
    UNIQUE KEY uk_audit_archive_storage_path (storage_relative_path),
    KEY idx_audit_archive_tenant_application_created (tenant_id, application_id, created_at),
    CONSTRAINT fk_audit_archive_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_audit_archive_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS audit_archive_item (
    archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    audit_row_id BIGINT UNSIGNED NOT NULL,
    occurred_month INT UNSIGNED NOT NULL,
    purged_at DATETIME(3) NULL,
    PRIMARY KEY (archive_id, audit_row_id, occurred_month),
    KEY idx_audit_archive_item_pending_purge (archive_id, purged_at, audit_row_id),
    CONSTRAINT fk_audit_archive_item_archive FOREIGN KEY (archive_id) REFERENCES audit_archive (archive_id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS audit_dead_letter (
    dead_letter_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload MEDIUMBLOB NOT NULL,
    last_error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_error_message VARCHAR(1000) NOT NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    replayed_at DATETIME(3) NULL,
    PRIMARY KEY (dead_letter_id),
    KEY idx_audit_dead_letter_tenant_status_created (tenant_id, status, created_at),
    KEY idx_audit_dead_letter_tenant_application_status (tenant_id, application_code, status, created_at),
    KEY idx_audit_dead_letter_tenant_event (tenant_id, event_id),
    CONSTRAINT fk_audit_dead_letter_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
