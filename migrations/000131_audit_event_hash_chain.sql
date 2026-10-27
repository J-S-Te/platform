-- 审计 P1 · 防篡改哈希链（000131）。
--
-- 每行事件携带链式摘要：chain_hash = H(chain_prev_hash || payload_hash)，
-- 其中 H 与 payload_hash_alg 同一算法/同一平台密钥（HMAC 配置密钥时，否则 SHA-256 回退），
-- 链首以全零 prev 起链。行内同时保存 chain_prev_hash，使留存清理删除旧行后，
-- 剩余行仍可各自自证（重算自身链摘要），窗口内还可做相邻连续性校验。
-- 链头表按（租户、应用、环境）持有一条 last_* 记录，摄取事务内 FOR UPDATE 串行化，
-- 防止并发摄取造成链分叉；校验按 chain_sequence 连续性识别缺口（留存清理造成的缺口
-- 记为 GAP，不是“链断裂”）。
ALTER TABLE audit_event
  ADD COLUMN chain_prev_hash BINARY(32) NULL AFTER payload_hash_alg,
  ADD COLUMN chain_hash BINARY(32) NULL AFTER chain_prev_hash,
  ADD COLUMN chain_sequence BIGINT UNSIGNED NULL AFTER chain_hash;

CREATE TABLE audit_event_chain (
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  last_event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  last_chain_hash BINARY(32) NOT NULL,
  last_sequence BIGINT UNSIGNED NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (tenant_id, application_id, environment_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
