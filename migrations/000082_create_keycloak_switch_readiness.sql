-- Per application-environment cutover evidence.  These values are written only
-- by server-side synchronisation and authenticated broker-login verification.
CREATE TABLE IF NOT EXISTS keycloak_switch_readiness (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_ready BOOLEAN NOT NULL DEFAULT FALSE,
    role_catalog_synced BOOLEAN NOT NULL DEFAULT FALSE,
    user_projection_completed BOOLEAN NOT NULL DEFAULT FALSE,
    broker_login_verified BOOLEAN NOT NULL DEFAULT FALSE,
    broker_verified_identity_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    broker_verified_issuer VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL,
    broker_verified_client_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    broker_verified_by_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    broker_verified_session_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    broker_verified_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, application_id, environment_id),
    CONSTRAINT fk_keycloak_switch_readiness_application
        FOREIGN KEY (tenant_id, application_id) REFERENCES platform_application (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_switch_readiness_environment
        FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Append-only evidence makes a successful gate independently auditable even
-- when a later verification supersedes the current snapshot.
CREATE TABLE IF NOT EXISTS keycloak_broker_login_verification (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    identity_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    issuer VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    keycloak_client_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    verified_by_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    verified_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_keycloak_broker_verification_scope (tenant_id, application_id, environment_id, verified_at),
    CONSTRAINT fk_keycloak_broker_verification_user
        FOREIGN KEY (tenant_id, identity_id) REFERENCES iam_user (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_broker_verification_application
        FOREIGN KEY (tenant_id, application_id) REFERENCES platform_application (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_broker_verification_environment
        FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
