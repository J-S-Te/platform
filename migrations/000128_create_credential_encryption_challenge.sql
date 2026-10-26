CREATE TABLE IF NOT EXISTS credential_encryption_challenge (
    id_hash BINARY(32) NOT NULL,
    kid VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    purpose VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(64) NOT NULL DEFAULT '',
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    binding_hash BINARY(32) NOT NULL,
    http_method VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    route_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    PRIMARY KEY (id_hash),
    KEY idx_credential_challenge_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
