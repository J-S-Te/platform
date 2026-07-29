-- Extend the original development-environment correction from migration 000052 to every
-- contract_management environment without changing the already-applied historical migration.
-- This keeps existing databases and fresh installations on the same public route.

UPDATE platform_application
SET homepage_url = 'http://localhost:8081/contract_management/dashboard',
    updated_at = UTC_TIMESTAMP(3)
WHERE tenant_id = '01J00000000000000000000000'
  AND code = 'contract_management';

UPDATE platform_application_environment AS environment
JOIN platform_application AS application
  ON application.id = environment.application_id
 AND application.tenant_id = environment.tenant_id
SET environment.path_prefix = '/contract_management',
    environment.updated_at = UTC_TIMESTAMP(3)
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management';

UPDATE platform_application_login_target AS target
JOIN platform_application AS application
  ON application.id = target.application_id
 AND application.tenant_id = target.tenant_id
SET target.target_uri = '/contract_management/dashboard',
    target.updated_at = UTC_TIMESTAMP(3)
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
  AND target.target_code = 'home';
