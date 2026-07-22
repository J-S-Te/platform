-- OIDC/OAuth protocol runtime state. Secret values are never stored directly: authorization
-- codes, refresh tokens and revoked bearer tokens are all persisted only as SHA-256 digests.

CREATE TABLE IF NOT EXISTS oauth_authorization_code (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_hash BINARY(32) NOT NULL,
    redirect_uri VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    nonce VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    code_challenge VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    code_challenge_method VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_authorization_code_hash (code_hash),
    KEY idx_oauth_authorization_code_client (tenant_id, oauth_client_id, status, expires_at),
    KEY idx_oauth_authorization_code_session (tenant_id, session_id, status, expires_at),
    KEY idx_oauth_authorization_code_user (tenant_id, user_id, status, expires_at),
    CONSTRAINT fk_oauth_authorization_code_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_authorization_code_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_authorization_code_session FOREIGN KEY (session_id) REFERENCES iam_session (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_authorization_code_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_authorization_code_user FOREIGN KEY (user_id) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS oauth_token_family (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Scope is immutable for the complete refresh-token family. Rotated token rows inherit it
    -- by joining this table, so a refresh can never silently expand or drop authorization scope.
    scope VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    authorized_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    revoked_at DATETIME(3) NULL,
    revoke_reason VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    KEY idx_oauth_token_family_client (tenant_id, oauth_client_id, status, expires_at),
    KEY idx_oauth_token_family_session (tenant_id, session_id, status, expires_at),
    KEY idx_oauth_token_family_user (tenant_id, user_id, status, expires_at),
    CONSTRAINT fk_oauth_token_family_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_token_family_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_token_family_session FOREIGN KEY (session_id) REFERENCES iam_session (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_token_family_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_token_family_user FOREIGN KEY (user_id) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS oauth_refresh_token (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_family_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    parent_refresh_token_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    token_hash BINARY(32) NOT NULL,
    issued_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    used_at DATETIME(3) NULL,
    revoked_at DATETIME(3) NULL,
    revoke_reason VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_refresh_token_hash (token_hash),
    KEY idx_oauth_refresh_token_family (tenant_id, token_family_id, status, expires_at),
    KEY idx_oauth_refresh_token_client (tenant_id, oauth_client_id, status, expires_at),
    CONSTRAINT fk_oauth_refresh_token_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_refresh_token_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_refresh_token_family FOREIGN KEY (token_family_id) REFERENCES oauth_token_family (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_refresh_token_parent FOREIGN KEY (parent_refresh_token_id) REFERENCES oauth_refresh_token (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS oauth_token_revocation (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_hash BINARY(32) NOT NULL,
    token_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revoked_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NULL,
    reason VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_token_revocation_hash (tenant_id, token_hash),
    KEY idx_oauth_token_revocation_lookup (tenant_id, token_type, expires_at),
    CONSTRAINT fk_oauth_token_revocation_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_token_revocation_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
