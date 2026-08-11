-- Keycloak's OIDC identity-provider broker does not send a PKCE challenge to
-- the upstream platform OAuth client. Keep PKCE mandatory for business-facing
-- browser clients, but allow the dedicated server-managed broker client to
-- complete the authorization-code callback.
UPDATE platform_oauth_client
SET require_pkce = 0,
    updated_at = CURRENT_TIMESTAMP
WHERE client_id = 'keycloak-broker'
  AND status = 'ACTIVE';
