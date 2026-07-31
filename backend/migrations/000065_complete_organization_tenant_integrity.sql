-- Complete the tenant-safe identity and organization relationships introduced by migration 000055.
-- Existing primary keys remain globally unique; these composite keys exist so every relationship
-- carrying tenant_id can be enforced by InnoDB without relying on application-layer validation.
--
-- The migration intentionally does not rewrite inconsistent rows. If historical data contains a
-- cross-tenant manager, leader, account-user, or session-account reference, adding the relevant
-- foreign key fails instead of silently discarding or reassigning security-sensitive data.
--
-- This project uses forward-only migrations. If an operator must manually roll back this migration,
-- drop constraints before their supporting indexes, in this order:
--   ALTER TABLE iam_session DROP FOREIGN KEY fk_session_account_tenant;
--   ALTER TABLE iam_account DROP FOREIGN KEY fk_account_user_tenant;
--   ALTER TABLE iam_org_unit DROP FOREIGN KEY fk_org_leader_tenant;
--   ALTER TABLE iam_user DROP FOREIGN KEY fk_user_manager_tenant;
--   ALTER TABLE iam_org_unit DROP INDEX idx_org_tenant_leader;
--   ALTER TABLE iam_account DROP INDEX uq_account_tenant_id;
--   ALTER TABLE iam_membership DROP INDEX uq_membership_tenant_id;

-- Make memberships tenant-addressable for future tenant-safe references. iam_position already gains
-- uq_position_tenant_id in migration 000061; iam_user and iam_org_unit gain equivalent keys in 000055.
ALTER TABLE iam_membership
    ADD UNIQUE KEY uq_membership_tenant_id (tenant_id, id);

-- A manager must be a user in the same tenant. idx_user_manager from migration 000003 already
-- provides the required child-side index for this foreign key.
ALTER TABLE iam_user
    ADD CONSTRAINT fk_user_manager_tenant
        FOREIGN KEY (tenant_id, manager_user_id)
        REFERENCES iam_user (tenant_id, id)
        ON DELETE RESTRICT;

-- Organization leaders must belong to the same tenant as the organization they lead.
ALTER TABLE iam_org_unit
    ADD KEY idx_org_tenant_leader (tenant_id, leader_user_id),
    ADD CONSTRAINT fk_org_leader_tenant
        FOREIGN KEY (tenant_id, leader_user_id)
        REFERENCES iam_user (tenant_id, id)
        ON DELETE RESTRICT;

-- Accounts and sessions participate in the user identity chain. Enforcing the tenant on both links
-- prevents an account from pointing at another tenant's user and a session from authenticating an
-- account owned by another tenant.
ALTER TABLE iam_account
    ADD UNIQUE KEY uq_account_tenant_id (tenant_id, id),
    ADD CONSTRAINT fk_account_user_tenant
        FOREIGN KEY (tenant_id, user_id)
        REFERENCES iam_user (tenant_id, id)
        ON DELETE RESTRICT;

ALTER TABLE iam_session
    ADD CONSTRAINT fk_session_account_tenant
        FOREIGN KEY (tenant_id, account_id)
        REFERENCES iam_account (tenant_id, id)
        ON DELETE RESTRICT;
