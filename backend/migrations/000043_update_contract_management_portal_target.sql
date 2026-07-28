-- Point the dynamically loaded portal card at the browser UI hosted by the shared frontend.
UPDATE platform_application
SET homepage_url = 'http://localhost:8081/contract-management/dashboard',
    updated_at = UTC_TIMESTAMP(3)
WHERE tenant_id = '01J00000000000000000000000'
  AND code = 'contract_management';

UPDATE platform_application_login_target AS target
JOIN platform_application AS application
  ON application.id = target.application_id
 AND application.tenant_id = target.tenant_id
SET target.target_uri = '/contract-management/dashboard',
    target.updated_at = UTC_TIMESTAMP(3)
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
  AND target.target_code = 'home';
