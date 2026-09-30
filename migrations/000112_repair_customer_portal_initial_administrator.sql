-- customer_portal historically declared initial_admin_roles: []. The onboarding state was still
-- marked as assigned, so a later Update/Retry could not grant the now-supported internal
-- portal_super_admin role. Reset only the one-time completion marker when that exact effective
-- binding is absent; the controlled deployment workflow will publish the catalog first and then
-- perform the auditable, idempotent role assignment. This migration never grants a role itself.
UPDATE subsystem_deployment_state AS deployment
JOIN platform_application AS application
  ON application.id = deployment.application_id
 AND application.tenant_id = deployment.tenant_id
SET deployment.initial_access_assigned_at = NULL,
    deployment.updated_at = UTC_TIMESTAMP(3)
WHERE application.code = 'customer_portal'
  AND deployment.application_code = 'customer_portal'
  AND deployment.environment_code = 'prod'
  AND deployment.initial_access_assigned_at IS NOT NULL
  AND (
      deployment.initial_admin_user_id IS NULL
      OR NOT EXISTS (
          SELECT 1
          FROM authz_role_binding AS binding
          JOIN authz_role AS role
            ON role.id = binding.role_id
           AND role.tenant_id = binding.tenant_id
           AND role.application_id = binding.application_id
          WHERE binding.tenant_id = deployment.tenant_id
            AND binding.application_id = deployment.application_id
            AND binding.subject_type = 'USER'
            AND binding.subject_id = deployment.initial_admin_user_id
            AND binding.scope_type = 'TENANT'
            AND binding.scope_id = ''
            AND binding.status = 'ACTIVE'
            AND (binding.valid_from IS NULL OR binding.valid_from <= UTC_TIMESTAMP(3))
            AND (binding.valid_until IS NULL OR binding.valid_until > UTC_TIMESTAMP(3))
            AND role.code = 'portal_super_admin'
            AND role.role_type = 'APPLICATION'
            AND role.status = 'ACTIVE'
      )
  );
