-- Customer binding: the platform-held mapping between an external portal
-- identity and the CRM-owned customer reference. The reference is stored only
-- as a keyed HMAC-SM3 digest (uniqueness/lookup) and an AES-256-GCM ciphertext
-- (server-side emission through authorization-context). No plaintext customer
-- reference is ever written to this table or returned by management APIs.
CREATE TABLE IF NOT EXISTS iam_external_customer_binding (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    identity_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    customer_ref_digest BINARY(32) NOT NULL,
    customer_ref_cipher VARBINARY(256) NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_customer_binding_identity (tenant_id, identity_id, application_code),
    UNIQUE KEY uk_customer_binding_customer (tenant_id, customer_ref_digest),
    KEY idx_customer_binding_status (tenant_id, application_code, status, updated_at),
    CONSTRAINT fk_customer_binding_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_customer_binding_identity FOREIGN KEY (identity_id) REFERENCES iam_external_identity (id) ON DELETE RESTRICT,
    CONSTRAINT chk_customer_binding_application CHECK (application_code = 'customer_portal'),
    CONSTRAINT chk_customer_binding_status CHECK (status IN ('ACTIVE', 'DISABLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Platform-held external customer binding; SM3 digest for lookup, AES-GCM ciphertext for controlled emission.';

-- Extend the machine-write idempotency ledger with the two binding operations.
ALTER TABLE iam_external_identity_idempotency
    DROP CHECK chk_external_identity_idempotency_operation;

ALTER TABLE iam_external_identity_idempotency
    ADD CONSTRAINT chk_external_identity_idempotency_operation
        CHECK (operation IN ('PROVISION', 'ASSIGN_ROLE', 'REVOKE_ROLE', 'BIND', 'DISABLE_BIND'));
