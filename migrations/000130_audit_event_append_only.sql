-- 审计 P0 · 只追加约束（000130）：以触发器在数据库层拒绝审计事件的改写与越权删除。
--
-- 背景：审计事件此前只有应用层“只追加”的契约注释（见 audit/application/service.go），
-- 任何能写库的账号（含 DBA）都可以 UPDATE / DELETE audit_event 而不留痕。
-- 本迁移把“只追加”落成数据库层硬约束。迁移执行器逐语句执行、不支持 DELIMITER，
-- 因此触发器体保持单条语句：
--   * audit_event_no_update：无条件 SIGNAL——事件落库后永不改写（脱敏在写入前完成）。
--   * audit_event_purge_guard：DELETE 仅当该行已在 audit_archive_item 中被留存任务标记为
--     “已清理”（purged_at IS NOT NULL，即归档副本已核验、PurgeArchivedEvents 在同一事务内
--     先标记后删除）时放行；否则守卫子查询返回两行、抛 MySQL 1242
--     （“Subquery returns more than 1 row”）。对 1242 的解释即本注释：有人绕过留存任务
--     直接删除审计事件。守卫不依赖会话变量，豁免不会随连接池复用泄漏；归档副本始终保留完整数据。
CREATE TRIGGER audit_event_no_update
  BEFORE UPDATE ON audit_event
  FOR EACH ROW
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_event is append-only: UPDATE rejected';

CREATE TRIGGER audit_event_purge_guard
  BEFORE DELETE ON audit_event
  FOR EACH ROW
  SET @audit_retention_guard = IF(
    EXISTS (
      SELECT 1 FROM audit_archive_item item
       WHERE item.audit_row_id = OLD.id
         AND item.occurred_month = OLD.occurred_month
         AND item.purged_at IS NOT NULL
    ),
    0,
    (SELECT guard.reason
       FROM (SELECT 'audit_event DELETE requires a verified retention archive purge mark' AS reason) AS guard
       JOIN (SELECT 1 AS one UNION ALL SELECT 2) AS duplicate)
  );
