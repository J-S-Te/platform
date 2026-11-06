-- 审计去重键补环境维度（000133）。
--
-- 背景：audit_event_dedup 原主键为 (application_id, event_id)，同一应用在不同环境
-- 使用相同 event_id（环境克隆、共用发布批次）时后到的环境被当成 DUPLICATE 丢弃，
-- 但发布方仍收到成功回执，造成跨环境审计事件永久丢失。
-- 本迁移把去重维度改为 (application_id, environment_id, event_id)。
--
-- 历史数据不丢行：audit_row_id（配合 occurred_month 定位事件行）给出 environment_id；
-- 事件行已被留存清理（归档后删除）、行缺失或月份不一致时无法还原环境，这些历史去重行
-- 保留在空环境（''）下，既不删除也不再参与任何真实环境的去重判定——它们本来就不该继续
-- 阻止某个环境摄取同一 event_id（宁可让历史歧义行放弃去重，也不能丢失新事件）。
-- 回填带 environment_id = '' 条件，可安全重跑；列与主键变更都按 000093 的方式用
-- information_schema 守卫，DDL 中途失败后可整体重试。
SET @migration_sql := IF(
    EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'audit_event_dedup' AND column_name = 'environment_id'),
    'SELECT 1',
    'ALTER TABLE audit_event_dedup ADD COLUMN environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '''' AFTER application_id'
);
PREPARE migration_stmt FROM @migration_sql;
EXECUTE migration_stmt;
DEALLOCATE PREPARE migration_stmt;

UPDATE audit_event_dedup AS dedup
  JOIN audit_event AS event
    ON event.id = dedup.audit_row_id
   AND event.occurred_month = dedup.occurred_month
   SET dedup.environment_id = event.environment_id
 WHERE dedup.environment_id = '';

SET @migration_sql := IF(
    EXISTS (
        SELECT 1 FROM information_schema.statistics
         WHERE table_schema = DATABASE() AND table_name = 'audit_event_dedup'
           AND index_name = 'PRIMARY' AND column_name = 'environment_id'
    ),
    'SELECT 1',
    'ALTER TABLE audit_event_dedup DROP PRIMARY KEY, ADD PRIMARY KEY (application_id, environment_id, event_id)'
);
PREPARE migration_stmt FROM @migration_sql;
EXECUTE migration_stmt;
DEALLOCATE PREPARE migration_stmt;
