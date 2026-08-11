-- Records the immutable, one-time initialization of the first platform super administrator.
-- No default human account or password is seeded. The operator-supplied password is hashed with
-- Argon2id by the controlled bootstrap endpoint before it reaches this table.
CREATE TABLE IF NOT EXISTS iam_bootstrap_state (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    first_super_admin_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    first_super_admin_account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    initialized_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_iam_bootstrap_state_tenant (tenant_id),
    UNIQUE KEY uk_iam_bootstrap_state_account (first_super_admin_account_id),
    KEY idx_iam_bootstrap_state_user (first_super_admin_user_id),
    CONSTRAINT fk_iam_bootstrap_state_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_iam_bootstrap_state_user FOREIGN KEY (first_super_admin_user_id) REFERENCES iam_user (id) ON DELETE RESTRICT,
    CONSTRAINT fk_iam_bootstrap_state_account FOREIGN KEY (first_super_admin_account_id) REFERENCES iam_account (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
