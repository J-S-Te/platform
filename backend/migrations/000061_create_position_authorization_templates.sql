-- Introduce position authorization templates without materializing per-user grants.
-- Effective authorization is inherited through active iam_membership rows. A membership can opt
-- out of that inheritance, while manual and system grants remain independent.
ALTER TABLE iam_membership
    ADD COLUMN inherit_authorization TINYINT(1) NOT NULL DEFAULT 1 AFTER is_primary;

-- Preserve authorization provenance on the canonical binding table. Empty origin identifiers are
-- deliberate sentinels for MANUAL and SYSTEM rows; TEMPLATE rows use the assignment and template
-- item IDs so multiple sources can grant the same role without overwriting one another.
ALTER TABLE authz_role_binding
    ADD COLUMN grant_origin VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'MANUAL' AFTER status,
    ADD COLUMN origin_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER grant_origin,
    ADD COLUMN origin_item_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER origin_id;

-- Existing bindings predate provenance and therefore remain MANUAL, except the built-in ordinary
-- platform-user grant, which is created and maintained by the platform itself.
UPDATE authz_role_binding AS binding
JOIN authz_role AS role
  ON role.id = binding.role_id
 AND role.tenant_id = binding.tenant_id
 AND role.application_id = binding.application_id
JOIN platform_application AS application
  ON application.id = binding.application_id
 AND application.tenant_id = binding.tenant_id
SET binding.grant_origin = 'SYSTEM'
WHERE binding.subject_type = 'USER'
  AND role.code = 'platform-user'
  AND role.built_in = 1
  AND application.code = 'platform';

ALTER TABLE authz_role_binding
    DROP INDEX uk_role_binding,
    ADD UNIQUE KEY uk_role_binding_origin (
        tenant_id,
        application_id,
        role_id,
        subject_type,
        subject_id,
        scope_type,
        scope_id,
        grant_origin,
        origin_id,
        origin_item_id
    ),
    ADD KEY idx_role_binding_origin (tenant_id, grant_origin, origin_id, status);

-- Composite identifiers are added only to support tenant-safe foreign keys in the template model.
-- They do not change the existing globally unique primary-key semantics.
ALTER TABLE platform_application
    ADD UNIQUE KEY uq_application_tenant_id (tenant_id, id);

ALTER TABLE authz_role
    ADD UNIQUE KEY uq_authz_role_tenant_application_id (tenant_id, application_id, id);

ALTER TABLE iam_position
    ADD UNIQUE KEY uq_position_tenant_id (tenant_id, id);

CREATE TABLE IF NOT EXISTS authz_position_grant_template (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(1000) NULL,
    valid_from DATETIME(3) NULL,
    valid_until DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_position_grant_template_code (tenant_id, code),
    UNIQUE KEY uq_position_grant_template_tenant_id (tenant_id, id),
    KEY idx_position_grant_template_status (tenant_id, status, valid_from, valid_until),
    CONSTRAINT fk_position_grant_template_tenant
        FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Reusable position-to-application-role authorization template.';

CREATE TABLE IF NOT EXISTS authz_position_grant_template_role (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    role_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scope_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'TENANT',
    scope_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    valid_from DATETIME(3) NULL,
    valid_until DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_position_grant_template_role (
        tenant_id,
        template_id,
        application_id,
        role_id,
        scope_type,
        scope_id
    ),
    UNIQUE KEY uq_position_grant_template_role_tenant_id (tenant_id, id),
    KEY idx_position_grant_template_role_application (tenant_id, application_id, status),
    KEY idx_position_grant_template_role_role (tenant_id, application_id, role_id),
    CONSTRAINT fk_position_grant_template_role_tenant
        FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_position_grant_template_role_template
        FOREIGN KEY (tenant_id, template_id)
        REFERENCES authz_position_grant_template (tenant_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT fk_position_grant_template_role_application
        FOREIGN KEY (tenant_id, application_id)
        REFERENCES platform_application (tenant_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT fk_position_grant_template_role_role
        FOREIGN KEY (tenant_id, application_id, role_id)
        REFERENCES authz_role (tenant_id, application_id, id)
        ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Application and role mappings contained in a position authorization template.';

CREATE TABLE IF NOT EXISTS authz_position_grant_template_assignment (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    position_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    valid_from DATETIME(3) NULL,
    valid_until DATETIME(3) NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_position_grant_template_assignment (tenant_id, position_id, template_id),
    UNIQUE KEY uq_position_grant_template_assignment_tenant_id (tenant_id, id),
    KEY idx_position_grant_template_assignment_template (tenant_id, template_id, status),
    KEY idx_position_grant_template_assignment_position (tenant_id, position_id, status, valid_from, valid_until),
    CONSTRAINT fk_position_grant_template_assignment_tenant
        FOREIGN KEY (tenant_id) REFERENCES iam_tenant (id) ON DELETE RESTRICT,
    CONSTRAINT fk_position_grant_template_assignment_position
        FOREIGN KEY (tenant_id, position_id)
        REFERENCES iam_position (tenant_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT fk_position_grant_template_assignment_template
        FOREIGN KEY (tenant_id, template_id)
        REFERENCES authz_position_grant_template (tenant_id, id)
        ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='Effective-dated assignments of authorization templates to organization positions.';
