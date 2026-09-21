ALTER TABLE file_object
    ADD COLUMN namespace VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER application_id,
    ADD COLUMN purpose VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER namespace,
    ADD COLUMN policy_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER purpose,
    ADD COLUMN authenticated_client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER policy_version,
    ADD COLUMN retention_class VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LONG_TERM' AFTER authenticated_client_id,
    ADD COLUMN retention_until DATETIME(3) NULL AFTER retention_class,
    ADD KEY idx_file_namespace_purpose (tenant_id, namespace, purpose, created_at);

ALTER TABLE file_version
    ADD COLUMN validated_at DATETIME(3) NULL AFTER created_at;

CREATE TABLE IF NOT EXISTS file_upload_v2_session (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    authenticated_client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    namespace VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    purpose VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    policy_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_name VARCHAR(512) NOT NULL,
    declared_media_type VARCHAR(255) NOT NULL,
    classification VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expected_size BIGINT UNSIGNED NOT NULL,
    expected_sha256 BINARY(32) NOT NULL,
    resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    binding_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(512) NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    upload_mode VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ticket_hash BINARY(32) NULL,
    ticket_expires_at DATETIME(3) NULL,
    ticket_used_at DATETIME(3) NULL,
    retention_until DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    failure_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    completed_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_file_upload_v2_idempotency (tenant_id, application_id, idempotency_key),
    UNIQUE KEY uk_file_upload_v2_file (file_id),
    KEY idx_file_upload_v2_expiry (status, ticket_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS file_validation_event (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    validator VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    validator_version VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_file_validation_event (tenant_id, file_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS file_download_ticket (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_hash BINARY(32) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    used_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_file_download_ticket_hash (token_hash),
    KEY idx_file_download_ticket_expiry (expires_at, used_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS file_access_audit (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    authenticated_client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    action VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_file_access_audit_file (tenant_id, file_id, created_at),
    KEY idx_file_access_audit_application (tenant_id, application_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
