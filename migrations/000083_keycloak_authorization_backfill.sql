-- Persist the work generated when an application environment first gains a
-- Keycloak Client mapping.  The outbox remains the retry mechanism; these
-- ledgers make creation of that work idempotent across retried Admin requests.
CREATE TABLE IF NOT EXISTS keycloak_authorization_reconcile_backfill (
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    identity_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    outbox_event_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (tenant_id, identity_id, application_id, environment_id),
    UNIQUE KEY uk_keycloak_reconcile_backfill_event (outbox_event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Pre-environment events were deliberately left with environment_id NULL by
-- 000080.  Record every source-event/target-environment expansion exactly
-- once before retiring the source event, so retries and concurrent syncs do
-- not duplicate or endlessly fan out legacy work.
CREATE TABLE IF NOT EXISTS keycloak_authorization_outbox_expansion (
    source_outbox_event_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    outbox_event_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (source_outbox_event_id, environment_id),
    UNIQUE KEY uk_keycloak_outbox_expansion_event (outbox_event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
