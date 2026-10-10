-- First directory adoption declared refresh_token but omitted its TTL. Repair
-- only canonical, tenant/environment-bound platform browser clients whose
-- existing grants already permit refresh. Preserve intentional nonzero TTLs,
-- service clients, Keycloak context clients, credentials and all business data.
UPDATE platform_oauth_client AS c
JOIN platform_application AS a ON a.id = c.application_id AND a.tenant_id = c.tenant_id
JOIN platform_application_environment AS e
  ON e.id = c.environment_id AND e.application_id = a.id AND e.tenant_id = c.tenant_id
SET c.refresh_token_ttl_seconds = 2592000,
    c.version = c.version + 1,
    c.updated_at = UTC_TIMESTAMP(3)
WHERE c.refresh_token_ttl_seconds = 0
  AND c.client_type = 'confidential' AND c.token_auth_method = 'client_secret_basic'
  AND c.require_pkce = 1 AND c.status = 'ACTIVE'
  AND a.code IN ('contract_management','project_management','customer_and_opportunity','customer_portal','settlement','data_analysis')
  AND c.client_id = CONCAT(LOWER(a.code), '-', LOWER(e.environment), '-web')
  AND (e.issuer_alias IS NULL OR LOWER(TRIM(e.issuer_alias)) IN ('','platform','basic_platform'))
  AND EXISTS (SELECT 1 FROM platform_oauth_grant_type AS g WHERE g.oauth_client_id = c.id AND g.grant_type = 'authorization_code')
  AND EXISTS (SELECT 1 FROM platform_oauth_grant_type AS g WHERE g.oauth_client_id = c.id AND g.grant_type = 'refresh_token')
  AND EXISTS (SELECT 1 FROM platform_oauth_client_scope AS s WHERE s.oauth_client_id = c.id AND s.scope_code = 'openid');
