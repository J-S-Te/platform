-- External identities introduced before the login-account reservation contract may not yet have
-- an iam_account row. The repair path uses only platform-owned identifiers and creates no password
-- credential; password initialization remains an explicit platform-administrator action.
INSERT INTO iam_account (
    id,
    tenant_id,
    user_id,
    username,
    account_type,
    auth_source,
    locked_until,
    last_login_at,
    status,
    version,
    created_at,
    created_by,
    updated_at,
    updated_by
)
SELECT
    identity.id,
    identity.tenant_id,
    identity.platform_user_id,
    identity.account_no,
    'HUMAN',
    'LOCAL',
    NULL,
    NULL,
    CASE WHEN identity.status = 'DISABLED' THEN 'DISABLED' ELSE 'ACTIVE' END,
    1,
    identity.created_at,
    identity.created_by,
    identity.updated_at,
    identity.updated_by
FROM iam_external_identity AS identity
WHERE NOT EXISTS (
    SELECT 1
    FROM iam_account AS account
    WHERE account.tenant_id = identity.tenant_id
      AND account.user_id = identity.platform_user_id
);

-- Make the one-login-account-per-external-subject contract a database invariant. Existing normal
-- users may still own multiple authentication sources, so the constraint belongs on the external
-- identity table and references the account by the same tenant and user.
ALTER TABLE iam_external_identity
    ADD COLUMN login_account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER account_no;

UPDATE iam_external_identity AS identity
JOIN iam_account AS account
  ON account.tenant_id = identity.tenant_id
 AND account.user_id = identity.platform_user_id
 AND account.username = identity.account_no
SET identity.login_account_id = account.id
WHERE identity.login_account_id IS NULL;

ALTER TABLE iam_external_identity
    MODIFY COLUMN login_account_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ADD UNIQUE KEY uk_external_identity_login_account (tenant_id, login_account_id),
    ADD CONSTRAINT fk_external_identity_login_account
        FOREIGN KEY (tenant_id, login_account_id)
        REFERENCES iam_account (tenant_id, id)
        ON DELETE RESTRICT;
