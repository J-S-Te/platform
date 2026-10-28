-- 将向导任务与其实际领取的部署代次绑定，禁止恢复时借用最新代次收口。
CREATE TABLE IF NOT EXISTS subsystem_access_deployment_attempt (
    tenant_id CHAR(26) NOT NULL,
    operation_id CHAR(26) NOT NULL,
    application_id CHAR(26) NOT NULL,
    environment_id CHAR(26) NOT NULL,
    generation BIGINT UNSIGNED NOT NULL,
    phase VARCHAR(16) NOT NULL,
    initial_runtime_provisioning BOOLEAN NOT NULL DEFAULT FALSE,
    runtime_expectations JSON NULL,
    candidate_snapshot VARCHAR(128) NOT NULL DEFAULT '',
    manifest_checksum VARCHAR(128) NOT NULL DEFAULT '',
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT chk_access_deployment_phase CHECK (phase IN ('PREPARED', 'DISPATCHED', 'SUCCEEDED', 'FAILED', 'RETRY_REQUESTED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
