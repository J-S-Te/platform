CREATE TABLE IF NOT EXISTS keycloak_application_client_mapping (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    realm VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    keycloak_client_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_synced_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, application_id, environment_id),
    UNIQUE KEY uk_keycloak_client_mapping_client (tenant_id, realm, keycloak_client_id),
    CONSTRAINT fk_keycloak_client_mapping_application FOREIGN KEY (tenant_id, application_id) REFERENCES platform_application (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_keycloak_client_mapping_environment FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT,
    CONSTRAINT chk_keycloak_client_mapping_status CHECK (status IN ('PENDING', 'SYNCED', 'FAILED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
