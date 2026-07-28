-- Distinguish any authenticated request from an explicit browser interaction.
-- Idle-session enforcement uses last_interactive_at; last_seen_at remains observability data.
ALTER TABLE iam_session
    ADD COLUMN last_interactive_at DATETIME(3) NULL AFTER last_seen_at;

UPDATE iam_session
SET last_interactive_at = last_seen_at
WHERE last_interactive_at IS NULL;

ALTER TABLE iam_session
    MODIFY COLUMN last_interactive_at DATETIME(3) NOT NULL,
    ADD KEY idx_session_last_interactive (tenant_id, account_id, status, last_interactive_at);
