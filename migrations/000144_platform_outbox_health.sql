-- 子系统 outbox 最新健康快照；平台库无法直接读取子系统本地 outbox，必须由 worker 主动上报。
CREATE TABLE IF NOT EXISTS platform_outbox_health (
    application_code VARCHAR(64) NOT NULL,
    environment_code VARCHAR(64) NOT NULL,
    pending BIGINT UNSIGNED NOT NULL,
    dead BIGINT UNSIGNED NOT NULL,
    oldest_at DATETIME(3) NULL,
    reported_at DATETIME(3) NOT NULL,
    PRIMARY KEY (application_code, environment_code),
    KEY idx_platform_outbox_health_reported (reported_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
