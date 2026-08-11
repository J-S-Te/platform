-- Enforce the tenant and organization relationships already checked by the application layer.
-- Composite keys make a cross-tenant ID or a position from another organization impossible to
-- persist through a script, batch job, or future code path that bypasses those application checks.

-- MySQL must see the referenced composite index before the self-referencing foreign key is
-- parsed. Keep these as separate ALTER statements; combining them fails with error 1215.
ALTER TABLE iam_org_unit
    ADD UNIQUE KEY uq_org_tenant_id (tenant_id, id);

ALTER TABLE iam_org_unit
    ADD CONSTRAINT fk_org_parent_tenant
        FOREIGN KEY (tenant_id, parent_id)
        REFERENCES iam_org_unit (tenant_id, id)
        ON DELETE RESTRICT;

ALTER TABLE iam_user
    ADD UNIQUE KEY uq_user_tenant_id (tenant_id, id),
    ADD KEY idx_user_tenant_primary_org (tenant_id, primary_org_id),
    ADD CONSTRAINT fk_user_primary_org_tenant
        FOREIGN KEY (tenant_id, primary_org_id)
        REFERENCES iam_org_unit (tenant_id, id)
        ON DELETE RESTRICT;

ALTER TABLE iam_position
    ADD UNIQUE KEY uq_position_tenant_org_id (tenant_id, org_unit_id, id),
    ADD CONSTRAINT fk_position_org_tenant
        FOREIGN KEY (tenant_id, org_unit_id)
        REFERENCES iam_org_unit (tenant_id, id)
        ON DELETE RESTRICT;

ALTER TABLE iam_membership
    ADD KEY idx_membership_tenant_org_position (tenant_id, org_unit_id, position_id),
    ADD CONSTRAINT fk_membership_user_tenant
        FOREIGN KEY (tenant_id, user_id)
        REFERENCES iam_user (tenant_id, id)
        ON DELETE RESTRICT,
    ADD CONSTRAINT fk_membership_org_tenant
        FOREIGN KEY (tenant_id, org_unit_id)
        REFERENCES iam_org_unit (tenant_id, id)
        ON DELETE RESTRICT,
    ADD CONSTRAINT fk_membership_position_org_tenant
        FOREIGN KEY (tenant_id, org_unit_id, position_id)
        REFERENCES iam_position (tenant_id, org_unit_id, id)
        ON DELETE RESTRICT;
