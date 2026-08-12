-- issuer_alias is the single authentication-provider selector for an
-- application environment.  Historic Basic Platform rows used NULL, blank,
-- mixed case, or basic_platform; normalize those safe equivalents before the
-- column becomes non-null and constrained.
UPDATE platform_application_environment
SET issuer_alias = CASE
    WHEN issuer_alias IS NULL OR TRIM(issuer_alias) = '' THEN 'platform'
    WHEN LOWER(TRIM(issuer_alias)) IN ('platform', 'basic_platform') THEN 'platform'
    WHEN LOWER(TRIM(issuer_alias)) = 'keycloak' THEN 'keycloak'
    ELSE issuer_alias
END;

ALTER TABLE platform_application_environment
    MODIFY COLUMN issuer_alias VARCHAR(128) NOT NULL DEFAULT 'platform';

ALTER TABLE platform_application_environment
    ADD CONSTRAINT chk_platform_application_environment_issuer_alias
    CHECK (issuer_alias IN ('platform', 'keycloak'));
