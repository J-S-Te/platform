ALTER TABLE iam_personnel_handover_item
  ADD COLUMN completed_by CHAR(26) NULL AFTER status,
  ADD COLUMN completed_at DATETIME(3) NULL AFTER completed_by;

CREATE TABLE IF NOT EXISTS iam_personnel_change_transition (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id CHAR(26) NOT NULL,
  request_id CHAR(26) NOT NULL,
  from_status VARCHAR(32) NOT NULL DEFAULT '',
  to_status VARCHAR(32) NOT NULL,
  operator_id CHAR(26) NOT NULL,
  reference_value VARCHAR(500) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_personnel_change_transition_request (tenant_id, request_id, id),
  CONSTRAINT fk_personnel_change_transition_request
    FOREIGN KEY (request_id) REFERENCES iam_personnel_change_request(id)
    ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Preserve an auditable baseline for requests created before transition history
-- existed. It records the imported current state without inventing intermediate
-- approvals.
INSERT INTO iam_personnel_change_transition (
  tenant_id, request_id, from_status, to_status, operator_id, reference_value, created_at
)
SELECT
  request.tenant_id,
  request.id,
  '',
  request.status,
  COALESCE(request.approved_by, request.submitted_by),
  COALESCE(request.handover_reference, request.approval_reference, request.rejection_reason),
  request.updated_at
FROM iam_personnel_change_request AS request
WHERE NOT EXISTS (
  SELECT 1 FROM iam_personnel_change_transition AS transition_log
  WHERE transition_log.tenant_id = request.tenant_id
    AND transition_log.request_id = request.id
);

-- Requests already waiting for handover must not be stranded after the new
-- fail-closed rule is deployed.
INSERT IGNORE INTO iam_personnel_handover_item (
  id, tenant_id, request_id, system_code, resource_type, resource_id,
  current_owner_id, target_owner_id, status, created_at, updated_at
)
SELECT
  request.id,
  request.tenant_id,
  request.id,
  'platform',
  'PERSONNEL_RESPONSIBILITY',
  request.user_id,
  request.user_id,
  NULL,
  'PENDING',
  request.updated_at,
  request.updated_at
FROM iam_personnel_change_request AS request
WHERE request.change_type = 'TERMINATION'
  AND request.status = 'PENDING_HANDOVER';
