-- 统一子系统接入向导：操作与阶段记录（只保存非秘密信息）与单目标并发槽位。
-- 与 subsystem_deployment_state 的关系：该表继续表示"部署状态"，本组表表示"向导操作进度"，
-- 二者共用部署 generation 与并发保护；本组表只保存进度与证据引用，不保存任何密钥材料或完整命令。

CREATE TABLE IF NOT EXISTS subsystem_access_operation (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  application_id CHAR(26) NULL,
  environment_id CHAR(26) NULL,
  application_code VARCHAR(64) NOT NULL,
  environment_code VARCHAR(16) NOT NULL,
  action VARCHAR(16) NOT NULL,
  stage VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL,
  preview_version VARCHAR(64) NOT NULL,
  request_digest CHAR(64) NOT NULL,
  -- 执行参数（非敏感）：由部署 Agent 与目录登记使用；内部服务地址/组件/命令不在请求中。
  public_origin VARCHAR(255) NOT NULL DEFAULT '',
  auth_provider VARCHAR(64) NOT NULL DEFAULT '',
  initial_admin_id CHAR(26) NULL,
  idempotency_key VARCHAR(160) NOT NULL,
  operator_id CHAR(26) NOT NULL,
  lease_owner VARCHAR(64) NULL,
  lease_expires_at DATETIME(3) NULL,
  attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
  error_code VARCHAR(128) NULL,
  error_message VARCHAR(1000) NULL,
  result_ref VARCHAR(255) NULL,
  -- 仅在进行中（PENDING/RUNNING/WAITING）时写入 tenant:application:environment；
  -- 结束或失败后置空，从而允许同一目标发起后续操作。唯一索引保证同目标只有一个执行中操作。
  active_slot VARCHAR(160) NULL,
  version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_subsystem_access_operation_idempotency (tenant_id, idempotency_key),
  UNIQUE KEY uq_subsystem_access_operation_active (active_slot),
  KEY idx_subsystem_access_operation_target (tenant_id, application_code, environment_code, created_at),
  KEY idx_subsystem_access_operation_status (tenant_id, status, updated_at),
  KEY idx_subsystem_access_operation_lease (status, lease_expires_at),
  CONSTRAINT chk_subsystem_access_operation_action CHECK (action IN ('ONBOARD','ADOPT','RETRY','UPDATE')),
  CONSTRAINT chk_subsystem_access_operation_status CHECK (status IN ('PENDING','RUNNING','WAITING','FAILED','SUCCEEDED')),
  CONSTRAINT chk_subsystem_access_operation_stage CHECK (stage IN ('CHECK','DIRECTORY','AUTH','DEPLOY','READINESS','RESULT'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS subsystem_access_operation_stage (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  operation_id CHAR(26) NOT NULL,
  stage VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL,
  attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
  error_code VARCHAR(128) NULL,
  error_message VARCHAR(1000) NULL,
  evidence_ref VARCHAR(255) NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_subsystem_access_operation_stage (tenant_id, operation_id, stage),
  KEY idx_subsystem_access_stage_status (tenant_id, status, updated_at),
  CONSTRAINT fk_subsystem_access_stage_operation FOREIGN KEY (operation_id) REFERENCES subsystem_access_operation (id) ON DELETE CASCADE,
  CONSTRAINT chk_subsystem_access_stage_name CHECK (stage IN ('CHECK','DIRECTORY','AUTH','DEPLOY','READINESS','RESULT')),
  CONSTRAINT chk_subsystem_access_stage_status CHECK (status IN ('PENDING','RUNNING','WAITING','FAILED','SUCCEEDED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
