-- 外部 OIDC/OAuth 登录状态：原始 state、nonce、PKCE verifier、客户端密钥及令牌绝不入库。
-- payload_ciphertext 仅保存应用层加密后的短期登录载荷；state_hash 用于回调时的一次性、原子消费。
CREATE TABLE IF NOT EXISTS iam_federated_login_state (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    state_hash BINARY(32) NOT NULL,
    payload_ciphertext BLOB NOT NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_federated_login_state_hash (state_hash),
    KEY idx_federated_login_state_expiry (status, expires_at),
    KEY idx_federated_login_state_tenant_provider (tenant_id, provider_code, status, expires_at),
    CONSTRAINT fk_federated_login_state_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_federated_login_state_provider FOREIGN KEY (tenant_id, provider_code)
        REFERENCES iam_federated_identity_provider (tenant_id, provider_code) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
