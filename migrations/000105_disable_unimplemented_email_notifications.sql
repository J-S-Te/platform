-- Email delivery has no configured provider or worker in the current platform topology.
-- Keep the existing column for API compatibility, but fail closed so stored settings do not
-- claim that a channel is active when no delivery can occur.
UPDATE notification_setting
SET email_enabled = 0,
    updated_at = UTC_TIMESTAMP(3),
    version = version + 1
WHERE email_enabled <> 0;
