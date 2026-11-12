-- 站内信收件箱软删除（000142）。
--
-- 背景：用户需要清理自己已读的站内信，但不能删除消息本体、影响其他收件人或丢失运营投递审计。
-- 采用 notification_delivery.deleted_at 软删除：收件箱列表/未读数/详情/已读操作统一排除
-- deleted_at 非空的投递；运营投递列表仍保留全部记录。
--
-- 索引：收件箱查询按 tenant_id + recipient_user_id + status 等值，再按 read_at 过滤已读，
-- 以 (tenant_id, recipient_user_id, status, deleted_at, read_at) 覆盖删除与未读统计路径。
-- 使用 information_schema 守卫，使 DDL 失败或文件被重放时不会因列/索引已存在而中断。
SET @migration_sql := IF(
    EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = DATABASE()
           AND table_name = 'notification_delivery'
           AND column_name = 'deleted_at'
    ),
    'SELECT 1',
    'ALTER TABLE notification_delivery ADD COLUMN deleted_at DATETIME(3) NULL AFTER read_at'
);
PREPARE migration_stmt FROM @migration_sql;
EXECUTE migration_stmt;
DEALLOCATE PREPARE migration_stmt;

SET @migration_sql := IF(
    EXISTS (
        SELECT 1 FROM information_schema.statistics
         WHERE table_schema = DATABASE()
           AND table_name = 'notification_delivery'
           AND index_name = 'idx_notification_delivery_inbox_active'
    ),
    'SELECT 1',
    'ALTER TABLE notification_delivery ADD KEY idx_notification_delivery_inbox_active (tenant_id, recipient_user_id, status, deleted_at, read_at)'
);
PREPARE migration_stmt FROM @migration_sql;
EXECUTE migration_stmt;
DEALLOCATE PREPARE migration_stmt;
