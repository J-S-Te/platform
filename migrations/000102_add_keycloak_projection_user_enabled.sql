-- Retain the last Keycloak enabled state so time-based account expiry can be reconciled.
ALTER TABLE keycloak_authorization_projection
    ADD COLUMN user_enabled BOOLEAN NOT NULL DEFAULT TRUE AFTER role_config_hash,
    ADD KEY idx_keycloak_projection_eligibility (status, user_enabled, updated_at);
