-- Stores the upstream identity subject for federated/Keycloak accounts.
-- Nullable for existing LOCAL accounts; reset operations fail closed when absent.
-- This is a versioned migration: the migration ledger prevents re-execution after success.
-- Avoid MySQL 8.0.29+'s optional IF NOT EXISTS syntax so older production MySQL versions
-- can apply the same migration. If a previous attempt failed before this statement,
-- rerunning the migration is safe because the DDL was not applied.
ALTER TABLE iam_account ADD COLUMN external_subject_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL;
CREATE UNIQUE INDEX uk_account_external_subject ON iam_account (tenant_id, auth_source, external_subject_id);
