-- Tenant-configurable inactivity timeout for unified browser sessions.
-- The identity service revokes every active tenant-account session when any session exceeds this limit.
ALTER TABLE sec_login_policy
    ADD COLUMN idle_timeout_seconds INT UNSIGNED NOT NULL DEFAULT 1800 AFTER failure_reset_window_seconds;
