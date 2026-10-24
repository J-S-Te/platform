-- Keycloak 登录事件采集游标（AUD-IP-2026 / 批次 3）。
--
-- 采集器原先每轮固定回看 5 分钟且无持久游标：停机超过窗口即永久漏采。
-- 游标按 realm 记录已采集到的最后事件时间与事件 id，失败计数用于采集异常告警。
CREATE TABLE IF NOT EXISTS keycloak_event_cursor (
    realm VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_event_time DATETIME(3) NOT NULL,
    last_event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    consecutive_failures INT UNSIGNED NOT NULL DEFAULT 0,
    last_error VARCHAR(500) NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (realm)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
