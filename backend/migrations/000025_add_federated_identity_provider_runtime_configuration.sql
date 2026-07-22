-- 为外部 OIDC/OAuth 身份提供商增加真实登录所需的运行时配置。
-- 密钥列仅保存应用层 AES-256-GCM 密文；不得写入明文客户端密钥、访问令牌或刷新令牌。
ALTER TABLE iam_federated_identity_provider
    ADD COLUMN client_id VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL AFTER issuer,
    ADD COLUMN callback_uri VARCHAR(2048) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL AFTER client_id,
    ADD COLUMN authorization_scopes JSON NULL AFTER callback_uri,
    ADD COLUMN client_secret_ciphertext BLOB NULL AFTER authorization_scopes,
    ADD COLUMN client_secret_updated_at DATETIME(3) NULL AFTER client_secret_ciphertext;
