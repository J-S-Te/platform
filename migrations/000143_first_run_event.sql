-- 新手导览最小遥测：只记录租户/用户标识与导览事件，不存 PII。
CREATE TABLE IF NOT EXISTS first_run_event (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(64) NOT NULL,
  user_id VARCHAR(64) NOT NULL,
  event_name VARCHAR(64) NOT NULL,
  guide_key VARCHAR(64) NOT NULL DEFAULT '',
  step_index INT NOT NULL DEFAULT 0,
  outcome VARCHAR(32) NOT NULL DEFAULT '',
  client_version VARCHAR(32) NOT NULL DEFAULT '',
  task_key VARCHAR(64) NOT NULL DEFAULT '',
  occurred_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_first_run_event_tenant_created (tenant_id, created_at),
  KEY idx_first_run_event_tenant_user_created (tenant_id, user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
