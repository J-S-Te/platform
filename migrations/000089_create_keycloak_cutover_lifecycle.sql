-- Durable per-environment Keycloak migration state.  The table is deliberately
-- independent from runtime deployment state: a healthy container is not proof
-- that its authentication cutover completed the required observation period.
CREATE TABLE IF NOT EXISTS keycloak_cutover_lifecycle (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    observation_started_at DATETIME(3) NULL,
    observation_ends_at DATETIME(3) NULL,
    switched_at DATETIME(3) NULL,
    rollback_deadline_at DATETIME(3) NULL,
    rolled_back_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, application_id, environment_id),
    CONSTRAINT chk_keycloak_cutover_lifecycle_status
        CHECK (status IN ('NOT_STARTED', 'OBSERVING', 'READY_TO_SWITCH', 'SWITCHED', 'ROLLED_BACK', 'FAILED')),
    CONSTRAINT fk_keycloak_cutover_lifecycle_application
        FOREIGN KEY (tenant_id, application_id) REFERENCES platform_application (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_cutover_lifecycle_environment
        FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Append-only, non-secret transition evidence for the console timeline and
-- incident investigation.  Payloads are intentionally not persisted here.
CREATE TABLE IF NOT EXISTS keycloak_cutover_timeline_event (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    summary VARCHAR(1024) NOT NULL,
    occurred_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_keycloak_cutover_timeline_scope (tenant_id, application_id, environment_id, occurred_at),
    CONSTRAINT fk_keycloak_cutover_timeline_application
        FOREIGN KEY (tenant_id, application_id) REFERENCES platform_application (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_cutover_timeline_environment
        FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
