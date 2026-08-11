-- 高风险操作 MFA 二次验证：仅保存 challenge/grant 的 SHA-256 摘要，原始令牌、TOTP 和恢复码绝不入库。
-- 每条记录绑定一次已认证会话；grant 只能在相同 tenant/account/session 下使用一次。

CREATE TABLE IF NOT EXISTS sec_mfa_step_up_grant (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    mfa_challenge_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    challenge_hash BINARY(32) NOT NULL,
    grant_hash BINARY(32) NULL,
    challenge_expires_at DATETIME(3) NOT NULL,
    grant_expires_at DATETIME(3) NULL,
    granted_at DATETIME(3) NULL,
    consumed_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_mfa_step_up_challenge_hash (challenge_hash),
    UNIQUE KEY uk_mfa_step_up_grant_hash (grant_hash),
    KEY idx_mfa_step_up_session (tenant_id, account_id, session_id, status, grant_expires_at),
    KEY idx_mfa_step_up_challenge_expiry (status, challenge_expires_at),
    CONSTRAINT fk_mfa_step_up_grant_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_step_up_grant_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_step_up_grant_session FOREIGN KEY (session_id) REFERENCES iam_session (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_step_up_grant_challenge FOREIGN KEY (mfa_challenge_id) REFERENCES iam_mfa_challenge (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
