-- Environment became part of the projection identity in 000080.  Preserve
-- historical output under a sentinel target, then allow one row per real
-- application environment going forward.
UPDATE keycloak_authorization_projection
SET environment_id = '00000000000000000000000000'
WHERE environment_id IS NULL;

ALTER TABLE keycloak_authorization_projection
    MODIFY COLUMN environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (tenant_id, identity_id, application_id, environment_id);
