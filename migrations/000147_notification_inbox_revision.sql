-- 收件箱变更版本和投递状态同事务提交，供独立 API 进程合并在线推送。
CREATE TABLE IF NOT EXISTS notification_inbox_revision (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    recipient_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, recipient_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 只索引确实延后可见的通知，使到期检查不扫描历史即时通知。
ALTER TABLE notification_delivery
    ADD COLUMN scheduled_visible_at DATETIME(3)
        GENERATED ALWAYS AS (
            CASE WHEN status = 'DELIVERED' AND deleted_at IS NULL
                AND remind_at > delivered_at THEN remind_at END
        ) VIRTUAL,
    ADD KEY idx_notification_scheduled_visible (tenant_id, recipient_user_id, scheduled_visible_at);

CREATE TRIGGER notification_inbox_revision_insert
    AFTER INSERT ON notification_delivery FOR EACH ROW
    INSERT INTO notification_inbox_revision (tenant_id, recipient_user_id, revision)
    SELECT NEW.tenant_id, NEW.recipient_user_id, 1
    WHERE NEW.status = 'DELIVERED' AND (NEW.remind_at IS NULL OR NEW.remind_at <= NEW.delivered_at
           OR NEW.remind_at <= UTC_TIMESTAMP(3))
    ON DUPLICATE KEY UPDATE revision = revision + 1;

CREATE TRIGGER notification_inbox_revision_update
    AFTER UPDATE ON notification_delivery FOR EACH ROW
    INSERT INTO notification_inbox_revision (tenant_id, recipient_user_id, revision)
    SELECT NEW.tenant_id, NEW.recipient_user_id, 1
    WHERE (NEW.status = 'DELIVERED' AND OLD.status <> 'DELIVERED'
           AND (NEW.remind_at IS NULL OR NEW.remind_at <= NEW.delivered_at
           OR NEW.remind_at <= UTC_TIMESTAMP(3)))
        OR (NEW.status = 'DELIVERED' AND
            (NOT (OLD.read_at <=> NEW.read_at) OR NOT (OLD.deleted_at <=> NEW.deleted_at)))
    ON DUPLICATE KEY UPDATE revision = revision + 1;
