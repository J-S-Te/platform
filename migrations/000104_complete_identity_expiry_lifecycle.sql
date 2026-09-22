-- Complete the identity validity model. NULL remains the explicit permanent value.
ALTER TABLE iam_user
    ADD COLUMN valid_until DATETIME(3) NULL AFTER status,
    ADD COLUMN expiry_processed_at DATETIME(3) NULL AFTER valid_until,
    ADD KEY idx_user_valid_until (tenant_id, status, valid_until, expiry_processed_at);

ALTER TABLE iam_account
    ADD COLUMN expiry_processed_at DATETIME(3) NULL AFTER valid_until,
    ADD KEY idx_account_expiry_processing (tenant_id, status, valid_until, expiry_processed_at);
