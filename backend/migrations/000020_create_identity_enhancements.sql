-- 身份增强能力：所有短期协议状态、同意和 MFA 挑战都由 MySQL 持久化。
-- 本迁移仅前向新增对象；不修改既有 OAuth/OIDC 表，也不保存令牌、TOTP 密钥或断言明文。

CREATE TABLE IF NOT EXISTS platform_oauth_post_logout_redirect_uri (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    post_logout_redirect_uri VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (oauth_client_id, post_logout_redirect_uri),
    CONSTRAINT fk_oauth_post_logout_redirect_client FOREIGN KEY (oauth_client_id)
        REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS oauth_client_assertion_replay (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    jti_hash BINARY(32) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (oauth_client_id, jti_hash),
    KEY idx_oauth_client_assertion_replay_expiry (expires_at),
    CONSTRAINT fk_oauth_client_assertion_replay_client FOREIGN KEY (oauth_client_id)
        REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS oauth_pushed_authorization_request (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_uri_hash BINARY(32) NOT NULL,
    redirect_uri VARCHAR(2048) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    scope VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state VARCHAR(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    nonce VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    code_challenge VARCHAR(256) CHARACTER SET ascii COLLATE ascii_bin NULL,
    code_challenge_method VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    request_object_hash BINARY(32) NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_par_request_uri_hash (request_uri_hash),
    KEY idx_oauth_par_client (tenant_id, oauth_client_id, status, expires_at),
    CONSTRAINT fk_oauth_par_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oauth_par_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_oidc_user_consent (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    granted_at DATETIME(3) NULL,
    revoked_at DATETIME(3) NULL,
    updated_at DATETIME(3) NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, user_id, oauth_client_id),
    KEY idx_oidc_user_consent_client (tenant_id, oauth_client_id, status),
    CONSTRAINT fk_oidc_user_consent_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oidc_user_consent_user FOREIGN KEY (user_id) REFERENCES iam_user (id) ON DELETE RESTRICT,
    CONSTRAINT fk_oidc_user_consent_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_mfa_totp_factor (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    secret_ciphertext VARBINARY(1024) NOT NULL,
    enrolled_at DATETIME(3) NULL,
    last_used_at DATETIME(3) NULL,
    last_accepted_counter BIGINT UNSIGNED NULL,
    disabled_at DATETIME(3) NULL,
    expires_at DATETIME(3) NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (id),
    KEY idx_mfa_totp_factor_account (tenant_id, account_id, status),
    CONSTRAINT fk_mfa_totp_factor_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_totp_factor_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_mfa_challenge (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    factor_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    challenge_hash BINARY(32) NOT NULL,
    attempt_count SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    max_attempts SMALLINT UNSIGNED NOT NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    verified_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_mfa_challenge_hash (challenge_hash),
    KEY idx_mfa_challenge_account (tenant_id, account_id, status, expires_at),
    CONSTRAINT fk_mfa_challenge_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_challenge_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_challenge_factor FOREIGN KEY (factor_id) REFERENCES iam_mfa_totp_factor (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_federated_identity_provider (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    issuer VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (id),
    UNIQUE KEY uk_federated_identity_provider_code (tenant_id, provider_code),
    UNIQUE KEY uk_federated_identity_provider_issuer (tenant_id, issuer),
    CONSTRAINT fk_federated_identity_provider_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_federated_identity_binding (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    subject_hash BINARY(32) NOT NULL,
    bound_at DATETIME(3) NOT NULL,
    unbound_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (id),
    UNIQUE KEY uk_federated_identity_subject (tenant_id, provider_id, subject_hash),
    KEY idx_federated_identity_user (tenant_id, user_id, status),
    CONSTRAINT fk_federated_identity_binding_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_federated_identity_binding_provider FOREIGN KEY (provider_id) REFERENCES iam_federated_identity_provider (id) ON DELETE RESTRICT,
    CONSTRAINT fk_federated_identity_binding_user FOREIGN KEY (user_id) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 客户端公钥与 MFA 恢复码独立持久化；客户端私钥、恢复码和 TOTP 明文绝不入库。
CREATE TABLE IF NOT EXISTS oauth_client_jwk (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    public_jwk JSON NOT NULL,
    algorithm VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
    valid_from DATETIME(3) NOT NULL,
    valid_until DATETIME(3) NULL,
    revoked_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_oauth_client_jwk_kid (oauth_client_id, key_id),
    KEY idx_oauth_client_jwk_active (oauth_client_id, status, valid_until),
    CONSTRAINT fk_oauth_client_jwk_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS iam_mfa_recovery_code (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    factor_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_hash BINARY(32) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_mfa_recovery_code_hash (factor_id, code_hash),
    KEY idx_mfa_recovery_code_factor (tenant_id, factor_id, consumed_at),
    CONSTRAINT fk_mfa_recovery_code_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_recovery_code_factor FOREIGN KEY (factor_id) REFERENCES iam_mfa_totp_factor (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
