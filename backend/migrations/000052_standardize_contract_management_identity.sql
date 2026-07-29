-- Contract management is rendered by the shared frontend at the underscore route.
-- Migration 000046 accidentally used /contract-management, which is not a registered browser
-- route. Keep the historical migration immutable and correct both the portal target and the
-- seeded development environment here.
UPDATE platform_application AS application
JOIN platform_application_environment AS environment
  ON environment.application_id = application.id
 AND environment.tenant_id = application.tenant_id
SET application.homepage_url = 'http://localhost:8081/contract_management/dashboard',
    application.updated_at = UTC_TIMESTAMP(3),
    environment.base_url = 'http://localhost:8081',
    environment.upstream_url = 'http://contract-api:8081',
    environment.path_prefix = '/contract_management',
    environment.updated_at = UTC_TIMESTAMP(3)
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
  AND environment.environment = 'dev';

UPDATE platform_application_login_target AS target
JOIN platform_application AS application
  ON application.id = target.application_id
 AND application.tenant_id = target.tenant_id
JOIN platform_application_environment AS environment
  ON environment.id = target.environment_id
 AND environment.application_id = target.application_id
 AND environment.tenant_id = target.tenant_id
SET target.target_uri = '/contract_management/dashboard',
    target.updated_at = UTC_TIMESTAMP(3)
WHERE application.tenant_id = '01J00000000000000000000000'
  AND application.code = 'contract_management'
  AND environment.environment = 'dev'
  AND target.target_code = 'home';
