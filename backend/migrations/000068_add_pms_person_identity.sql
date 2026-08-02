-- Persist the explicit platform-user to PMS-person binding used by subsystem OIDC claims.
-- This value is never derived from iam_user.id, employee_no, account id, or OIDC sub.
ALTER TABLE iam_user
    ADD COLUMN pms_person_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER employee_no,
    ADD UNIQUE KEY uk_user_pms_person_id (tenant_id, pms_person_id);
