-- Enable the single-account/single-terminal policy without preserving legacy concurrent browser
-- sessions. Existing sessions are revoked once during rollout so the invariant is effective
-- immediately instead of waiting for every old cookie to expire.
UPDATE iam_session
SET status = 'REVOKED',
    revoked_at = CURRENT_TIMESTAMP(3),
    revoke_reason = 'SINGLE_SESSION_POLICY_MIGRATION'
WHERE status = 'ACTIVE'
  AND revoked_at IS NULL;
