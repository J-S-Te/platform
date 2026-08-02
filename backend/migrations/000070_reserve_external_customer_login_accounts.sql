-- External customer provisioning now reserves a HUMAN/LOCAL login account without creating a
-- password credential. Existing external identities are repaired lazily on idempotent replay;
-- this version exists to preserve migration immutability and document the account-lifecycle
-- contract without rewriting the already applied 000069 migration.
SELECT 1;
