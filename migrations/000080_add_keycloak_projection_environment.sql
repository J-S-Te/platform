-- A Keycloak Client belongs to an application environment.  Projection state
-- therefore has to be tracked per environment rather than per application
-- only: one authorization change can fan out to dev/test/staging/prod clients.
ALTER TABLE keycloak_authorization_projection
    ADD COLUMN environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER application_id,
    ADD KEY idx_keycloak_projection_environment_status (tenant_id, application_id, environment_id, status, updated_at);

ALTER TABLE keycloak_authorization_outbox
    ADD COLUMN environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER application_id,
    ADD KEY idx_keycloak_outbox_environment_subject (tenant_id, identity_id, application_id, environment_id, status);

-- Old events intentionally retain NULL: the consumer treats it as a request
-- to resolve every mapped environment, so upgrading cannot silently drop work.
