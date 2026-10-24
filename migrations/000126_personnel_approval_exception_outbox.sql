-- 超管例外审批审计 outbox：审批状态、例外原因与待投递审计事实在同一事务写入。
-- 外部审计端由 worker 异步投递；事件 ID 稳定，重试由审计接收端按 event_id 幂等去重，
-- 因此投递失败不会要求回滚已经生效的审批，也不会丢失例外原因。

CREATE TABLE IF NOT EXISTS iam_personnel_approval_exception_outbox (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  request_id CHAR(26) NOT NULL,
  operator_id CHAR(26) NOT NULL,
  submitted_by CHAR(26) NOT NULL,
  subject_user_id CHAR(26) NOT NULL,
  to_status VARCHAR(32) NOT NULL,
  rule_hit VARCHAR(64) NOT NULL,
  exception_reason VARCHAR(500) NOT NULL,
  approval_reference VARCHAR(128) NOT NULL DEFAULT '',
  occurred_at DATETIME(3) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL,
  last_error VARCHAR(500) NULL,
  delivered_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_personnel_approval_exception_pending (status, next_attempt_at),
  KEY idx_personnel_approval_exception_request (tenant_id, request_id),
  CONSTRAINT fk_personnel_approval_exception_request
    FOREIGN KEY (request_id) REFERENCES iam_personnel_change_request(id) ON DELETE CASCADE,
  CONSTRAINT chk_personnel_approval_exception_status CHECK (status IN ('PENDING','DELIVERED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
