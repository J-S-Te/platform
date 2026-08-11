-- In-app notification templates, rendered messages and tenant-scoped inbox delivery state.
-- This migration intentionally excludes SMTP, email sending, SMS and Webhook delivery.

CREATE TABLE IF NOT EXISTS notification_template (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    current_version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_template_code (tenant_id, code),
    KEY idx_notification_template_status (tenant_id, status, updated_at),
    CONSTRAINT fk_notification_template_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS notification_template_version (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    title_template VARCHAR(500) NOT NULL,
    body_template TEXT NOT NULL,
    variables_json JSON NOT NULL,
    published_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_template_version (template_id, version),
    KEY idx_notification_template_version_current (tenant_id, template_id, version),
    CONSTRAINT fk_notification_template_version_template FOREIGN KEY (template_id) REFERENCES notification_template (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_template_version_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS notification_message (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_version_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    category VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    title VARCHAR(500) NOT NULL,
    content TEXT NOT NULL,
    target_url VARCHAR(1000) NOT NULL DEFAULT '',
    reference_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    reference_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_message_idempotency (tenant_id, idempotency_key),
    KEY idx_notification_message_template (tenant_id, template_id, created_at),
    CONSTRAINT fk_notification_message_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_message_template FOREIGN KEY (template_id) REFERENCES notification_template (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_message_template_version FOREIGN KEY (template_version_id) REFERENCES notification_template_version (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS notification_delivery (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    message_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    recipient_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    last_error VARCHAR(500) NOT NULL DEFAULT '',
    next_retry_at DATETIME(3) NULL,
    locked_until DATETIME(3) NULL,
    delivered_at DATETIME(3) NULL,
    read_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_delivery_recipient (message_id, recipient_user_id),
    KEY idx_notification_delivery_inbox (tenant_id, recipient_user_id, status, delivered_at),
    KEY idx_notification_delivery_retry (status, next_retry_at, locked_until),
    CONSTRAINT fk_notification_delivery_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_delivery_message FOREIGN KEY (message_id) REFERENCES notification_message (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_delivery_recipient FOREIGN KEY (recipient_user_id) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
