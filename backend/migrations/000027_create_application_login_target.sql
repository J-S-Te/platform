-- 应用登录目标是统一登录完成后的受控业务落地地址。
-- 它与 OAuth Client 的 redirect_uri 相互独立，不得用于授权码协议回调。
CREATE TABLE IF NOT EXISTS platform_application_login_target (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    target_uri VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_app_login_target_code (environment_id, target_code),
    UNIQUE KEY uk_app_login_target_uri (environment_id, target_uri),
    KEY idx_app_login_target_resolution (tenant_id, application_id, environment_id, status, target_code),
    CONSTRAINT fk_app_login_target_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_app_login_target_application FOREIGN KEY (application_id) REFERENCES platform_application (id) ON DELETE RESTRICT,
    CONSTRAINT fk_app_login_target_environment FOREIGN KEY (environment_id) REFERENCES platform_application_environment (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
