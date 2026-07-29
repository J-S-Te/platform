-- The management console currently authorizes its routes from tenant-wide, server-loaded
-- permission codes. Organization- and resource-scoped role bindings therefore cannot be enforced
-- consistently by those routes and must not be left active as a misleading or potentially unsafe
-- authorization contract. Preserve the rows for audit/history, but fail closed until a trusted
-- resource-scope policy enforcement point is available for every protected endpoint.
UPDATE authz_role_binding
SET status = 'DISABLED',
    updated_at = UTC_TIMESTAMP(3),
    version = version + 1
WHERE status = 'ACTIVE'
  AND scope_type <> 'TENANT';
