-- 审计 P0 · 防篡改（000129）：payload_hash 记录算法标签。
--
-- 旧实现以无键 SHA-256 摘要规范 payload，任何能写库者可改写事件并重算，不构成防篡改证据。
-- 现配置平台密钥（PLATFORM_AUDIT_HASH_KEY）时新事件写入 HMAC-SHA256（审计 P0），摘要验证
-- 依据本列标签选择算法：历史行保持 SHA-256（列默认值），新行按摄取进程实际算法写入。
-- 注意：本列只是“算法标签”，配合 /api/v1/audit/events/{event_id}/hash-check 重算比对使用；
-- 行级 UPDATE/DELETE 的只追加约束由 000130 迁移以触发器实施。
ALTER TABLE audit_event
  ADD COLUMN payload_hash_alg VARCHAR(16) NOT NULL DEFAULT 'SHA-256'
  AFTER payload_hash;
