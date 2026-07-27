-- Adds an account-level validity boundary. NULL means the login account is permanent.
ALTER TABLE iam_account
    ADD COLUMN valid_until DATETIME(3) NULL AFTER last_login_at,
    ADD KEY idx_account_valid_until (tenant_id, status, valid_until);
