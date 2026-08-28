-- OIDC Back-Channel Logout 1.0 state. Logout tokens are short-lived, audience-bound
-- events; delivery and replay state are durable so retries survive process restarts.
CREATE TABLE IF NOT EXISTS platform_oauth_backchannel_logout_uri (
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    logout_uri VARCHAR(2048) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (oauth_client_id),
    CONSTRAINT fk_oauth_backchannel_logout_uri_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_backchannel_logout_outbox (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    oauth_client_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    subject_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    jti CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(3) NOT NULL,
    locked_until DATETIME(3) NULL,
    last_error VARCHAR(255) CHARACTER SET utf8mb4 NULL,
    created_at DATETIME(3) NOT NULL,
    delivered_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_backchannel_outbox_jti (jti),
    UNIQUE KEY uk_backchannel_outbox_client_session (oauth_client_id, session_id),
    KEY idx_backchannel_outbox_due (status, next_attempt_at, locked_until),
    CONSTRAINT fk_backchannel_outbox_client FOREIGN KEY (oauth_client_id) REFERENCES platform_oauth_client (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS platform_oauth_backchannel_logout_replay (
    jti CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    audience VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    consumed_at DATETIME(3) NOT NULL,
    PRIMARY KEY (jti),
    KEY idx_backchannel_replay_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
