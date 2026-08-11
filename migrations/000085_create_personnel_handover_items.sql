-- Responsibility snapshots published by CRM, contract and approval adapters.
-- Business records stay in their owning subsystem; this table is only the
-- platform-owned, tenant-scoped handover control plane.
CREATE TABLE IF NOT EXISTS iam_personnel_handover_item (
  id CHAR(26) NOT NULL,
  tenant_id CHAR(26) NOT NULL,
  request_id CHAR(26) NOT NULL,
  system_code VARCHAR(64) NOT NULL,
  resource_type VARCHAR(64) NOT NULL,
  resource_id VARCHAR(128) NOT NULL,
  current_owner_id CHAR(26) NOT NULL,
  target_owner_id CHAR(26) NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'PENDING',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_personnel_handover_item (tenant_id, request_id, system_code, resource_type, resource_id),
  KEY idx_personnel_handover_owner (tenant_id, request_id, current_owner_id, status),
  CONSTRAINT chk_personnel_handover_status CHECK (status IN ('PENDING','TRANSFERRED','COMPLETED','BLOCKED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
