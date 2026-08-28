-- 独立 File Gateway 数据库的首个可执行迁移必须自包含，不能依赖平台主库中的
-- iam_tenant 或 platform_application。所有跨系统标识仅按业务字段保存。
CREATE TABLE IF NOT EXISTS file_object (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_name VARCHAR(512) NOT NULL,
    file_extension VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
    media_type VARCHAR(255) NULL,
    classification VARCHAR(32) NOT NULL,
    owner_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    owner_org_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    current_version_no INT UNSIGNED NOT NULL DEFAULT 0,
    current_version_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id), KEY idx_file_app_time (tenant_id, application_id, created_at),
    KEY idx_file_owner (tenant_id, owner_user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS file_version (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version_no INT UNSIGNED NOT NULL,
    storage_relative_path VARCHAR(1000) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    size_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    -- PENDING_UPLOAD 阶段尚未完成对象存储写入；MarkValidating 会在状态推进为
    -- VALIDATING 的同一事务中写入真实大小与 SHA-256。
    sha256 BINARY(32) NULL,
    media_type VARCHAR(255) NULL,
    original_name VARCHAR(512) NOT NULL,
    uploader_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    upload_request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    upload_request_hash BINARY(32) NULL,
    status VARCHAR(32) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id), UNIQUE KEY uk_file_version (file_id, version_no),
    UNIQUE KEY uk_file_storage_path (storage_relative_path),
    KEY idx_file_upload_request (file_id, upload_request_id),
    KEY idx_file_upload_request_tenant (upload_request_id, upload_request_hash),
    CONSTRAINT fk_file_version_file FOREIGN KEY (file_id) REFERENCES file_object (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 上传会话是幂等请求的唯一仲裁点；后续仓储写入必须在同一事务中预留该记录。
CREATE TABLE IF NOT EXISTS file_upload_session (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    version_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_file_upload_session (tenant_id, application_id, request_id),
    KEY idx_file_upload_session_file (file_id),
    CONSTRAINT fk_file_upload_session_file FOREIGN KEY (file_id) REFERENCES file_object (id) ON DELETE SET NULL,
    CONSTRAINT fk_file_upload_session_version FOREIGN KEY (version_id) REFERENCES file_version (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS file_binding (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    binding_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    display_name VARCHAR(512) NULL,
    sort_order INT NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id), UNIQUE KEY uk_file_binding (application_id, resource_type, resource_id, file_id, binding_type),
    KEY idx_file_binding_resource (tenant_id, application_id, resource_type, resource_id, status),
    CONSTRAINT fk_file_binding_file FOREIGN KEY (file_id) REFERENCES file_object (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS async_job (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    parent_job_id BIGINT UNSIGNED NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    application_scope CHAR(26) CHARACTER SET ascii COLLATE ascii_bin
        GENERATED ALWAYS AS (COALESCE(application_id, '00000000000000000000000000')) STORED,
    job_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    aggregate_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    aggregate_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    idempotency_key VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    payload JSON NOT NULL, request_hash BINARY(32) NULL,
    request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    trace_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    business_ref VARCHAR(255) NULL, status VARCHAR(32) NOT NULL,
    priority INT NOT NULL DEFAULT 100, available_at DATETIME(3) NOT NULL,
    locked_by VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    locked_at DATETIME(3) NULL, last_attempt_at DATETIME(3) NULL,
    attempts INT UNSIGNED NOT NULL DEFAULT 0, retry_count INT UNSIGNED NOT NULL DEFAULT 0,
    max_attempts INT UNSIGNED NOT NULL DEFAULT 3,
    last_error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    last_error_message VARCHAR(1000) NULL, result_file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(3) NOT NULL, completed_at DATETIME(3) NULL, last_succeeded_at DATETIME(3) NULL,
    PRIMARY KEY (id), UNIQUE KEY uk_async_job_public_id (public_id),
    UNIQUE KEY uk_async_job_idempotency (tenant_id, application_scope, job_type, idempotency_key),
    KEY idx_job_poll (status, available_at, priority, id), KEY idx_job_lock (status, locked_at),
    KEY idx_job_aggregate (aggregate_type, aggregate_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
