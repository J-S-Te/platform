-- Bind every cutover gate and broker verification to the exact platform-managed
-- Keycloak Client projection configuration that produced the evidence.
ALTER TABLE keycloak_application_client_mapping
    ADD COLUMN configuration_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER keycloak_client_id;

ALTER TABLE keycloak_switch_readiness
    ADD COLUMN client_configuration_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER broker_login_verified,
    ADD COLUMN broker_verified_configuration_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER client_configuration_hash;

ALTER TABLE keycloak_broker_login_verification
    ADD COLUMN configuration_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER keycloak_client_id;

-- Existing evidence predates configuration binding. Fail closed and require a
-- fresh Client sync, full user reconcile and broker login verification.
UPDATE keycloak_switch_readiness
SET client_ready = FALSE,
    role_catalog_synced = FALSE,
    user_projection_completed = FALSE,
    broker_login_verified = FALSE,
    client_configuration_hash = '',
    broker_verified_configuration_hash = NULL,
    broker_verified_identity_id = NULL,
    broker_verified_issuer = NULL,
    broker_verified_client_id = NULL,
    broker_verified_by_id = NULL,
    broker_verified_session_id = NULL,
    broker_verified_at = NULL,
    updated_at = UTC_TIMESTAMP(3);

-- Removing only the idempotency ledger makes the next explicit Client sync
-- enqueue a new FULL_RECONCILE for every active identity. Historical outbox
-- and broker audit evidence remain immutable and queryable.
DELETE FROM keycloak_authorization_reconcile_backfill;

UPDATE keycloak_authorization_projection
SET status = 'PENDING',
    last_synced_at = NULL,
    last_error_code = NULL,
    last_error_message = NULL,
    updated_at = UTC_TIMESTAMP(3);
