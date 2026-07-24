CREATE TABLE IF NOT EXISTS platform_oauth_client (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_name VARCHAR(128) NOT NULL,
    client_type VARCHAR(32) NOT NULL,
    token_auth_method VARCHAR(64) NOT NULL,
    access_token_ttl_seconds INT UNSIGNED NOT NULL,
    refresh_token_ttl_seconds INT UNSIGNED NOT NULL,
    require_pkce TINYINT(1) NOT NULL DEFAULT 1,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_client_id (client_id),
    KEY idx_oauth_client_application (tenant_id, application_id, environment_id, status),
    CONSTRAINT fk_oauth_client_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_client_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_client_environment FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_redirect_uri (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    redirect_uri VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (oauth_client_id, redirect_uri),
    CONSTRAINT fk_oauth_redirect_uri_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_grant_type (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    grant_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (oauth_client_id, grant_type),
    CONSTRAINT fk_oauth_grant_type_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_client_scope (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (oauth_client_id, scope_code),
    CONSTRAINT fk_oauth_scope_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_client_credential (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_type VARCHAR(32) NOT NULL,
    secret_hash VARBINARY(255) NULL,
    public_key_jwk JSON NULL,
    fingerprint VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    valid_from DATETIME(3) NOT NULL,
    valid_until DATETIME(3) NULL,
    revoked_at DATETIME(3) NULL,
    status VARCHAR(32) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_credential_fingerprint (oauth_client_id, fingerprint),
    CONSTRAINT fk_oauth_credential_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
