-- MFA password-login pre-authentication records intentionally retain only a SHA-256 hash of a
-- high-entropy opaque credential. They bind the short-lived, single-use second-factor step to the
-- tenant and account whose password was just verified; no password, TOTP code, recovery code or
-- TOTP seed is persisted here.
CREATE TABLE IF NOT EXISTS iam_mfa_login_pre_auth (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    credential_hash BINARY(32) NOT NULL,
    attempt_count SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    max_attempts SMALLINT UNSIGNED NOT NULL,
    created_at DATETIME(3) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_mfa_login_pre_auth_credential_hash (credential_hash),
    KEY idx_mfa_login_pre_auth_account (tenant_id, account_id, status, expires_at),
    CONSTRAINT fk_mfa_login_pre_auth_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_mfa_login_pre_auth_account FOREIGN KEY (account_id) REFERENCES iam_account (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
