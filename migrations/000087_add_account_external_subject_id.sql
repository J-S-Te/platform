-- Stores the upstream identity subject for federated/Keycloak accounts.
-- Nullable for existing LOCAL accounts; reset operations fail closed when absent.
ALTER TABLE iam_account ADD COLUMN IF NOT EXISTS external_subject_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL;
CREATE UNIQUE INDEX uk_account_external_subject ON iam_account (tenant_id, auth_source, external_subject_id);
