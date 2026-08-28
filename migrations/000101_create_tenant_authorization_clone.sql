-- 新租户授权目录克隆的幂等控制面记录；只保存租户和请求状态，不保存用户绑定、凭据或环境秘密。
CREATE TABLE IF NOT EXISTS iam_tenant_authorization_clone (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_tenant_authz_clone_request (target_tenant_id, idempotency_key),
    KEY idx_tenant_authz_clone_target (target_tenant_id, status),
    CONSTRAINT fk_tenant_authz_clone_source FOREIGN KEY (source_tenant_id) REFERENCES iam_tenant(id) ON DELETE RESTRICT,
    CONSTRAINT fk_tenant_authz_clone_target FOREIGN KEY (target_tenant_id) REFERENCES iam_tenant(id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
