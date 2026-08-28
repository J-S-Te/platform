ALTER TABLE subsystem_deployment_state
    ADD COLUMN desired_manifest_checksum VARCHAR(80) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER attempt_count,
    ADD COLUMN applied_manifest_checksum VARCHAR(80) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER desired_manifest_checksum,
    ADD COLUMN manifest_drift_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'UNKNOWN' AFTER applied_manifest_checksum,
    ADD COLUMN manifest_last_applied_at DATETIME(3) NULL AFTER manifest_drift_status,
    ADD COLUMN manifest_last_verified_at DATETIME(3) NULL AFTER manifest_last_applied_at,
    ADD KEY idx_subsystem_manifest_drift (tenant_id, manifest_drift_status, updated_at);

UPDATE subsystem_deployment_state
SET manifest_drift_status = 'UNKNOWN'
WHERE desired_manifest_checksum IS NULL OR applied_manifest_checksum IS NULL;
