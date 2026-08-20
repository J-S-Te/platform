-- Asynchronous, application-authenticated ingestion for cross-system and high-priority
-- in-app notifications. Existing synchronous platform notifications remain supported.

ALTER TABLE notification_message
    MODIFY COLUMN template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    MODIFY COLUMN template_version_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN source_application VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER tenant_id,
    ADD COLUMN source_environment VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER source_application,
    ADD COLUMN source_event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER source_environment,
    ADD COLUMN event_type VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER source_event_id,
    ADD COLUMN notification_scope VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PLATFORM' AFTER event_type,
    ADD COLUMN priority VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'NORMAL' AFTER notification_scope,
    ADD COLUMN occurred_at DATETIME(3) NULL AFTER idempotency_key,
    ADD COLUMN expires_at DATETIME(3) NULL AFTER occurred_at,
    ADD KEY idx_notification_message_source_event (tenant_id, source_application, source_environment, source_event_id);

CREATE TABLE IF NOT EXISTS notification_event_inbox (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_application VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_environment VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload JSON NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    next_retry_at DATETIME(3) NULL,
    locked_until DATETIME(3) NULL,
    last_error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    last_error_message VARCHAR(500) NOT NULL DEFAULT '',
    message_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    received_at DATETIME(3) NOT NULL,
    processed_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_notification_event_inbox_source (tenant_id, source_application, source_environment, source_event_id),
    KEY idx_notification_event_inbox_claim (status, next_retry_at, locked_until, received_at),
    KEY idx_notification_event_inbox_tenant (tenant_id, received_at),
    CONSTRAINT fk_notification_event_inbox_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Initialise the materialised counter before the new write path starts updating it.
INSERT INTO notification_user_stat (tenant_id, user_id, unread_count, updated_at)
SELECT tenant_id, recipient_user_id, COUNT(*), UTC_TIMESTAMP(3)
FROM notification_delivery
WHERE status = 'DELIVERED' AND read_at IS NULL
GROUP BY tenant_id, recipient_user_id;

CREATE TABLE IF NOT EXISTS notification_user_stat (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    unread_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, user_id),
    CONSTRAINT fk_notification_user_stat_tenant FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_notification_user_stat_user FOREIGN KEY (user_id) REFERENCES iam_user (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The dedicated scope is intentionally separate from audit.ingest. OAuth clients are granted
-- this scope through the existing application-management path; no browser role is implied.
