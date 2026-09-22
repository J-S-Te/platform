-- Durable public HTTP/HTTPS transition coordinator.  The singleton row is the
-- authoritative current state; transition and resource rows are append-only
-- evidence used for restart recovery and exact rollback.  Certificate bodies,
-- private keys, tokens and client secrets are deliberately never persisted.
CREATE TABLE IF NOT EXISTS platform_public_transport_state (
    id TINYINT UNSIGNED NOT NULL,
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    active_mode VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    desired_mode VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    platform_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sso_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_platform_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NULL,
    target_sso_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NULL,
    active_transition_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    drain_until DATETIME(3) NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT chk_public_transport_singleton CHECK (id = 1),
    CONSTRAINT chk_public_transport_state CHECK (state IN ('HTTP','ENABLING_HTTPS','HTTPS','DISABLING_HTTPS')),
    CONSTRAINT chk_public_transport_active_mode CHECK (active_mode IN ('HTTP','HTTPS')),
    CONSTRAINT chk_public_transport_desired_mode CHECK (desired_mode IN ('HTTP','HTTPS'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE IF NOT EXISTS platform_public_transport_transition (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    from_mode VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_mode VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    phase VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_platform_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_platform_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_sso_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_sso_origin VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    certificate_fingerprint VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    certificate_not_after DATETIME(3) NULL,
    drain_until DATETIME(3) NULL,
    failure_stage VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    failure_message VARCHAR(1000) NULL,
    rollback_result VARCHAR(1000) NULL,
    started_at DATETIME(3) NOT NULL,
    completed_at DATETIME(3) NULL,
    failed_at DATETIME(3) NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_public_transport_transition_state (state, phase, updated_at),
    CONSTRAINT chk_public_transport_transition_mode CHECK (from_mode IN ('HTTP','HTTPS') AND target_mode IN ('HTTP','HTTPS'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_public_transport_resource (
    transition_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(191) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    source_value VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_value VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (transition_id, resource_type, resource_id, source_value),
    KEY idx_public_transport_resource_environment (environment_id, resource_type),
    CONSTRAINT fk_public_transport_resource_transition FOREIGN KEY (transition_id) REFERENCES platform_public_transport_transition (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
