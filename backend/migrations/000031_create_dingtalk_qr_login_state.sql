-- 钉钉第三方企业应用扫码状态：仅持久化 state 的 SHA-256 摘要和加密后的服务端载荷。
-- 原始 state、浏览器绑定值、授权码、AppSecret、上游访问令牌及钉钉身份标识绝不入库。
CREATE TABLE IF NOT EXISTS iam_dingtalk_qr_login_state (
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
    UNIQUE KEY uk_dingtalk_qr_login_state_hash (state_hash),
    KEY idx_dingtalk_qr_login_state_expiry (status, expires_at),
    KEY idx_dingtalk_qr_login_state_tenant_provider (tenant_id, provider_code, status, expires_at),
    CONSTRAINT fk_dingtalk_qr_login_state_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_dingtalk_qr_login_state_provider FOREIGN KEY (tenant_id, provider_code)
        REFERENCES iam_federated_identity_provider (tenant_id, provider_code) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
