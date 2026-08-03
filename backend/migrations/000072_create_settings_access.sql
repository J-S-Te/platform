-- Platform public-access configuration for the local unified orchestration.
-- 该配置描述统一前端对外公开地址与 OAuth HTTP 回调策略；"应用"动作由部署 Agent 写入
-- docker/.env.lan 覆盖文件并重建相关容器。生产部署不使用本配置，保持 localhost 语义。
CREATE TABLE IF NOT EXISTS settings_access (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    public_origin VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    allow_insecure_http_redirect TINYINT(1) NOT NULL DEFAULT 0,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_settings_access_tenant (tenant_id),
    CONSTRAINT fk_settings_access_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
