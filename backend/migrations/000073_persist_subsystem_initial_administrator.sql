ALTER TABLE subsystem_deployment_state
    ADD COLUMN initial_admin_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER environment_code,
	ADD COLUMN initial_access_assigned_at DATETIME(3) NULL AFTER initial_admin_user_id,
	ADD KEY idx_subsystem_deployment_initial_admin (tenant_id, initial_admin_user_id),
	ADD CONSTRAINT fk_subsystem_deployment_initial_admin
		FOREIGN KEY (tenant_id, initial_admin_user_id) REFERENCES iam_user (tenant_id, id) ON DELETE RESTRICT;

-- Existing rows predate this field. Their next retry safely falls back to the authenticated
-- operator. READY rows are treated as already completed so a later routine retry cannot
-- unexpectedly restore a role that an administrator intentionally removed.
UPDATE subsystem_deployment_state
SET initial_access_assigned_at = COALESCE(completed_at, updated_at)
WHERE status = 'READY';
