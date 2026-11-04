-- 保留失效锚点和上游sid墓碑，避免旧SSO会话重新获得八小时。
CREATE TABLE IF NOT EXISTS sgov_session (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    identity_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    authenticated_at DATETIME(3) NOT NULL,
    last_interactive_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    revoked_at DATETIME(3) NULL,
    revoke_reason VARCHAR(64) NOT NULL DEFAULT '',
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    KEY idx_sgov_identity (tenant_id, identity_id, revoked_at),
    KEY idx_sgov_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sgov_upstream (
    upstream_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    issuer VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sid VARCHAR(256) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject VARCHAR(256) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    identity_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    authenticated_at DATETIME(3) NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    KEY idx_sgov_upstream_session (session_id),
    UNIQUE KEY uk_sgov_session_issuer (session_id, issuer)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sgov_ticket (
    token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    nonce_hash VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    parent_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    KEY idx_sgov_ticket_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sgov_binding (
    token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    KEY idx_sgov_binding_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sgov_platform_link (
    iam_session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    KEY idx_sgov_platform_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
