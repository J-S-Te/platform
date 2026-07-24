-- DINGTALK_QR is not an OIDC issuer. Keep the existing unique key so OIDC issuers remain
-- tenant-unique, while permitting NULL issuer values for DingTalk QR providers.
-- client_id stores the DingTalk AppKey and client_secret_ciphertext stores only the encrypted
-- AppSecret; no plaintext credential or upstream subject is introduced by this migration.
ALTER TABLE iam_federated_identity_provider
    MODIFY COLUMN issuer VARCHAR(2048) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL;
