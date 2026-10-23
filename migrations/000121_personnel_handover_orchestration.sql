-- 人员交接编排：批次、系统冻结/适用性状态、逐项转移的幂等/租约/重试字段与真实回执。
-- 业务责任仍由各子系统持有，本组表只保存平台侧控制记录与回执，供受控对账与排期闸门使用。

CREATE TABLE IF NOT EXISTS iam_personnel_handover_batch (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  request_id CHAR(26) NOT NULL,
  user_id CHAR(26) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'DISCOVERING',
  cancel_requested_at DATETIME(3) NULL,
  completed_at DATETIME(3) NULL,
  version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_personnel_handover_batch_request (tenant_id, request_id),
  KEY idx_personnel_handover_batch_status (tenant_id, status, updated_at),
  CONSTRAINT chk_personnel_handover_batch_status CHECK (status IN ('DISCOVERING','FROZEN','TRANSFERRING','COMPLETED','CANCELLED','BLOCKED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS iam_personnel_handover_system (
  batch_id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  system_code VARCHAR(64) NOT NULL,
  capability VARCHAR(24) NOT NULL DEFAULT 'APPLICABLE',
  frozen TINYINT(1) NOT NULL DEFAULT 0,
  frozen_at DATETIME(3) NULL,
  discovered_items INT UNSIGNED NOT NULL DEFAULT 0,
  blocking_reason VARCHAR(128) NULL,
  completed_at DATETIME(3) NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (batch_id, system_code),
  KEY idx_personnel_handover_system_state (tenant_id, capability, frozen),
  CONSTRAINT fk_personnel_handover_system_batch
    FOREIGN KEY (batch_id) REFERENCES iam_personnel_handover_batch(id) ON DELETE CASCADE,
  CONSTRAINT chk_personnel_handover_system_capability CHECK (capability IN ('APPLICABLE','NOT_INSTALLED','UNREACHABLE','INCOMPATIBLE','LICENSE_DENIED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE iam_personnel_handover_item
  ADD COLUMN category VARCHAR(64) NOT NULL DEFAULT '' AFTER resource_id,
  ADD COLUMN resource_name VARCHAR(255) NOT NULL DEFAULT '' AFTER category,
  ADD COLUMN batch_id CHAR(26) NULL AFTER request_id,
  ADD COLUMN expected_version BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER target_owner_id,
  ADD COLUMN applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER expected_version,
  ADD COLUMN idempotency_key VARCHAR(160) NULL AFTER applied_version,
  ADD COLUMN lease_id CHAR(26) NULL AFTER idempotency_key,
  ADD COLUMN lease_deadline DATETIME(3) NULL AFTER lease_id,
  ADD COLUMN attempts INT UNSIGNED NOT NULL DEFAULT 0 AFTER lease_deadline,
  ADD COLUMN next_retry_at DATETIME(3) NULL AFTER attempts,
  ADD COLUMN last_error_code VARCHAR(64) NULL AFTER next_retry_at,
  ADD COLUMN evidence_ref VARCHAR(255) NULL AFTER last_error_code,
  ADD COLUMN version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER status,
  ADD UNIQUE KEY uq_personnel_handover_item_idempotency (idempotency_key),
  ADD KEY idx_personnel_handover_item_dispatch (tenant_id, status, next_retry_at, lease_deadline),
  ADD KEY idx_personnel_handover_item_batch (batch_id, system_code, status);

-- 既有状态约束只允许 PENDING/TRANSFERRED/COMPLETED/BLOCKED；编排需要 FAILED（终态失败）与
-- SKIPPED（责任已不存在，例如已被他人处理）。保留原取值以兼容既有数据。
ALTER TABLE iam_personnel_handover_item DROP CHECK chk_personnel_handover_status;
ALTER TABLE iam_personnel_handover_item
  ADD CONSTRAINT chk_personnel_handover_status CHECK (status IN ('PENDING','TRANSFERRED','COMPLETED','BLOCKED','FAILED','SKIPPED'));

CREATE TABLE IF NOT EXISTS iam_personnel_handover_receipt (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  batch_id CHAR(26) NOT NULL,
  item_id CHAR(26) NOT NULL,
  system_code VARCHAR(64) NOT NULL,
  idempotency_key VARCHAR(160) NOT NULL,
  status VARCHAR(24) NOT NULL,
  applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  reason_code VARCHAR(64) NULL,
  evidence_ref VARCHAR(255) NULL,
  received_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_personnel_handover_receipt_key (idempotency_key),
  KEY idx_personnel_handover_receipt_item (tenant_id, item_id, received_at),
  CONSTRAINT fk_personnel_handover_receipt_item
    FOREIGN KEY (item_id) REFERENCES iam_personnel_handover_item(id) ON DELETE CASCADE,
  CONSTRAINT chk_personnel_handover_receipt_status CHECK (status IN ('APPLIED','ALREADY_APPLIED','REJECTED','RETRYABLE'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
