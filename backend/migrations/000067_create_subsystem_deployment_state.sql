CREATE TABLE IF NOT EXISTS subsystem_deployment_state (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_code VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    last_error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    last_error_message VARCHAR(1000) NULL,
    started_at DATETIME(3) NULL,
    completed_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, environment_id),
    UNIQUE KEY uk_subsystem_deployment_environment (tenant_id, application_id, environment_id),
    KEY idx_subsystem_deployment_status (tenant_id, status, updated_at),
    CONSTRAINT fk_subsystem_deployment_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_subsystem_deployment_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_subsystem_deployment_environment FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Existing environments predate the deployment state machine. They are retained as READY so
-- the portal does not hide already-operated subsystems during the additive migration.
INSERT INTO subsystem_deployment_state (
    tenant_id, application_id, environment_id, application_code, environment_code,
    status, operation, generation, attempt_count, created_at, updated_at
)
SELECT
    application.tenant_id, application.id, environment.id, application.code, environment.environment,
    'READY', 'MIGRATION_BACKFILL', 1, 0, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)
FROM platform_application AS application
JOIN platform_application_environment AS environment
  ON environment.tenant_id = application.tenant_id
 AND environment.application_id = application.id
WHERE application.status = 'ACTIVE'
  AND environment.status = 'ACTIVE'
ON DUPLICATE KEY UPDATE environment_id = environment_id;
