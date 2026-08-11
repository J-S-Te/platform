-- Platform-owned, credential-free pre-provisioning state for external Portal customers.
-- iam_external_identity.platform_user_id is the stable OIDC subject (iam_user.id). No username,
-- password, token, secret, or activation credential is stored in these tables.
CREATE TABLE IF NOT EXISTS iam_external_identity (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    platform_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    account_no VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    email_digest BINARY(32) NULL,
    mobile_digest BINARY(32) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    activated_at DATETIME(3) NULL,
    disabled_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_external_identity_subject (tenant_id, platform_user_id),
    UNIQUE KEY uk_external_identity_account_no (tenant_id, account_no),
    UNIQUE KEY uk_external_identity_email (tenant_id, email_digest),
    UNIQUE KEY uk_external_identity_mobile (tenant_id, mobile_digest),
    KEY idx_external_identity_status (tenant_id, status, updated_at),
    CONSTRAINT fk_external_identity_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_external_identity_user FOREIGN KEY (tenant_id, platform_user_id) REFERENCES iam_user (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT chk_external_identity_locator CHECK (email_digest IS NOT NULL OR mobile_digest IS NOT NULL),
    CONSTRAINT chk_external_identity_status CHECK (status IN ('PENDING_ACTIVATION','ACTIVE','DISABLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Credential-free external customer identity reservation; activation remains owned by the platform account lifecycle.';

CREATE TABLE IF NOT EXISTS iam_external_identity_idempotency (
    id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    operation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    resource_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result_json VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    completed_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_external_identity_idempotency (tenant_id, oauth_client_id, operation, idempotency_key),
    KEY idx_external_identity_idempotency_resource (tenant_id, resource_id),
    CONSTRAINT fk_external_identity_idempotency_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_external_identity_idempotency_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT,
    CONSTRAINT fk_external_identity_idempotency_resource FOREIGN KEY (resource_id) REFERENCES iam_external_identity (id) ON DELETE RESTRICT,
    CONSTRAINT chk_external_identity_idempotency_operation CHECK (operation IN ('PROVISION','ASSIGN_ROLE','REVOKE_ROLE'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Stable application-bound replay result for external identity and role writes.';

CREATE TABLE IF NOT EXISTS iam_external_identity_nonce_replay (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    nonce_hash BINARY(32) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, nonce_hash),
    KEY idx_external_identity_nonce_expiry (expires_at),
    CONSTRAINT fk_external_identity_nonce_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Cross-instance replay guard for external identity management requests.';
