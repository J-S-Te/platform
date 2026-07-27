-- Basic Platform local test data.
--
-- Design rules:
-- 1. Reuse the existing default tenant, platform application, root organization and built-in platform-user role.
-- 2. Add only synthetic identity/organization data for local development; do not fabricate audit events or external systems.
-- 3. Do not create login accounts. In the current business model, users and login accounts have independent lifecycles.
-- 4. Use reserved example.invalid email addresses so test data cannot accidentally send real email.
-- 5. All inserts are idempotent and can be executed repeatedly.

SET NAMES utf8mb4;
SET time_zone = '+00:00';
START TRANSACTION;

SET @seed_now := UTC_TIMESTAMP(3);
SET @tenant_id := (SELECT id FROM iam_tenant WHERE code = 'default' LIMIT 1);
SET @application_id := (
    SELECT id
    FROM platform_application
    WHERE tenant_id = @tenant_id AND code = 'platform'
    LIMIT 1
);
SET @ordinary_role_id := (
    SELECT id
    FROM authz_role
    WHERE tenant_id = @tenant_id
      AND application_id = @application_id
      AND code = 'platform-user'
    LIMIT 1
);
SET @root_org_id := (
    SELECT id
    FROM iam_org_unit
    WHERE tenant_id = @tenant_id AND code = 'ROOT'
    LIMIT 1
);
SET @operator_user_id := COALESCE(
    (SELECT first_super_admin_user_id FROM iam_bootstrap_state WHERE tenant_id = @tenant_id LIMIT 1),
    (
        SELECT user_id
        FROM iam_account
        WHERE tenant_id = @tenant_id AND username = 'admin'
        LIMIT 1
    )
);

-- -----------------------------------------------------------------------------
-- Organization hierarchy
-- ROOT
-- ├── 技术中心
-- │   ├── 平台研发部
-- │   └── 运维保障部
-- └── 产品与运营中心
--     ├── 产品部
--     └── 客户成功部
-- -----------------------------------------------------------------------------
SET @tech_center_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'TECH_CENTER' LIMIT 1),
    '01KYDVHC000000000000000001'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @tech_center_id, @tenant_id, @root_org_id, 'TECH_CENTER', '技术中心', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @root_org_id), @tech_center_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @root_org_id),
    NULL, 100, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'TECH_CENTER'
);

SET @platform_rd_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PLATFORM_RD' LIMIT 1),
    '01KYDVHC000000000000000002'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @platform_rd_id, @tenant_id, @tech_center_id, 'PLATFORM_RD', '平台研发部', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @tech_center_id), @platform_rd_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @tech_center_id),
    NULL, 110, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PLATFORM_RD'
);

SET @ops_support_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'OPS_SUPPORT' LIMIT 1),
    '01KYDVHC000000000000000003'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @ops_support_id, @tenant_id, @tech_center_id, 'OPS_SUPPORT', '运维保障部', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @tech_center_id), @ops_support_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @tech_center_id),
    NULL, 120, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'OPS_SUPPORT'
);

SET @product_ops_center_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PRODUCT_OPS_CENTER' LIMIT 1),
    '01KYDVHC000000000000000004'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @product_ops_center_id, @tenant_id, @root_org_id, 'PRODUCT_OPS_CENTER', '产品与运营中心', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @root_org_id), @product_ops_center_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @root_org_id),
    NULL, 200, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PRODUCT_OPS_CENTER'
);

SET @product_dept_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PRODUCT_DEPT' LIMIT 1),
    '01KYDVHC000000000000000005'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @product_dept_id, @tenant_id, @product_ops_center_id, 'PRODUCT_DEPT', '产品部', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @product_ops_center_id), @product_dept_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @product_ops_center_id),
    NULL, 210, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'PRODUCT_DEPT'
);

SET @customer_success_id := COALESCE(
    (SELECT id FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'CUSTOMER_SUCCESS' LIMIT 1),
    '01KYDVHC000000000000000006'
);
INSERT INTO iam_org_unit (
    id, tenant_id, parent_id, code, name, org_type, path, depth,
    leader_user_id, sort_order, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @customer_success_id, @tenant_id, @product_ops_center_id, 'CUSTOMER_SUCCESS', '客户成功部', 'DEPARTMENT',
    CONCAT((SELECT path FROM iam_org_unit WHERE id = @product_ops_center_id), @customer_success_id, '/'),
    (SELECT depth + 1 FROM iam_org_unit WHERE id = @product_ops_center_id),
    NULL, 220, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_org_unit WHERE tenant_id = @tenant_id AND code = 'CUSTOMER_SUCCESS'
);

-- -----------------------------------------------------------------------------
-- Positions
-- -----------------------------------------------------------------------------
SET @backend_position_id := COALESCE(
    (
        SELECT id FROM iam_position
        WHERE tenant_id = @tenant_id AND org_unit_id = @platform_rd_id AND code = 'BACKEND_ENGINEER'
        LIMIT 1
    ),
    '01KYDVHC000000000000000007'
);
INSERT INTO iam_position (
    id, tenant_id, org_unit_id, code, name, position_level, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @backend_position_id, @tenant_id, @platform_rd_id, 'BACKEND_ENGINEER', '后端开发工程师', 'P4',
    'ACTIVE', 1, @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_position
    WHERE tenant_id = @tenant_id AND org_unit_id = @platform_rd_id AND code = 'BACKEND_ENGINEER'
);

SET @frontend_position_id := COALESCE(
    (
        SELECT id FROM iam_position
        WHERE tenant_id = @tenant_id AND org_unit_id = @platform_rd_id AND code = 'FRONTEND_ENGINEER'
        LIMIT 1
    ),
    '01KYDVHC000000000000000008'
);
INSERT INTO iam_position (
    id, tenant_id, org_unit_id, code, name, position_level, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @frontend_position_id, @tenant_id, @platform_rd_id, 'FRONTEND_ENGINEER', '前端开发工程师', 'P4',
    'ACTIVE', 1, @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_position
    WHERE tenant_id = @tenant_id AND org_unit_id = @platform_rd_id AND code = 'FRONTEND_ENGINEER'
);

SET @ops_position_id := COALESCE(
    (
        SELECT id FROM iam_position
        WHERE tenant_id = @tenant_id AND org_unit_id = @ops_support_id AND code = 'OPS_ENGINEER'
        LIMIT 1
    ),
    '01KYDVHC000000000000000009'
);
INSERT INTO iam_position (
    id, tenant_id, org_unit_id, code, name, position_level, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @ops_position_id, @tenant_id, @ops_support_id, 'OPS_ENGINEER', '运维工程师', 'P4',
    'ACTIVE', 1, @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_position
    WHERE tenant_id = @tenant_id AND org_unit_id = @ops_support_id AND code = 'OPS_ENGINEER'
);

SET @product_position_id := COALESCE(
    (
        SELECT id FROM iam_position
        WHERE tenant_id = @tenant_id AND org_unit_id = @product_dept_id AND code = 'PRODUCT_MANAGER'
        LIMIT 1
    ),
    '01KYDVHC00000000000000000A'
);
INSERT INTO iam_position (
    id, tenant_id, org_unit_id, code, name, position_level, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @product_position_id, @tenant_id, @product_dept_id, 'PRODUCT_MANAGER', '产品经理', 'P5',
    'ACTIVE', 1, @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_position
    WHERE tenant_id = @tenant_id AND org_unit_id = @product_dept_id AND code = 'PRODUCT_MANAGER'
);

SET @customer_success_position_id := COALESCE(
    (
        SELECT id FROM iam_position
        WHERE tenant_id = @tenant_id AND org_unit_id = @customer_success_id AND code = 'CUSTOMER_SUCCESS_SPECIALIST'
        LIMIT 1
    ),
    '01KYDVHC00000000000000000B'
);
INSERT INTO iam_position (
    id, tenant_id, org_unit_id, code, name, position_level, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @customer_success_position_id, @tenant_id, @customer_success_id,
    'CUSTOMER_SUCCESS_SPECIALIST', '客户成功专员', 'P3',
    'ACTIVE', 1, @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_position
    WHERE tenant_id = @tenant_id
      AND org_unit_id = @customer_success_id
      AND code = 'CUSTOMER_SUCCESS_SPECIALIST'
);

-- -----------------------------------------------------------------------------
-- Ordinary users. Employee numbers follow the current service rule: EMP-<USER_ULID>.
-- -----------------------------------------------------------------------------
SET @zhang_wei_id := COALESCE(
    (SELECT id FROM iam_user WHERE tenant_id = @tenant_id AND email = 'zhang.wei@example.invalid' LIMIT 1),
    '01KYDVHC00000000000000000C'
);
INSERT INTO iam_user (
    id, tenant_id, employee_no, display_name, legal_name, email,
    primary_org_id, manager_user_id, employment_status, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @zhang_wei_id, @tenant_id, CONCAT('EMP-', @zhang_wei_id), '张伟', '张伟', 'zhang.wei@example.invalid',
    NULL, NULL, 'EMPLOYED', 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_user WHERE tenant_id = @tenant_id AND email = 'zhang.wei@example.invalid'
);

SET @li_na_id := COALESCE(
    (SELECT id FROM iam_user WHERE tenant_id = @tenant_id AND email = 'li.na@example.invalid' LIMIT 1),
    '01KYDVHC00000000000000000D'
);
INSERT INTO iam_user (
    id, tenant_id, employee_no, display_name, legal_name, email,
    primary_org_id, manager_user_id, employment_status, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @li_na_id, @tenant_id, CONCAT('EMP-', @li_na_id), '李娜', '李娜', 'li.na@example.invalid',
    NULL, NULL, 'EMPLOYED', 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_user WHERE tenant_id = @tenant_id AND email = 'li.na@example.invalid'
);

SET @wang_qiang_id := COALESCE(
    (SELECT id FROM iam_user WHERE tenant_id = @tenant_id AND email = 'wang.qiang@example.invalid' LIMIT 1),
    '01KYDVHC00000000000000000E'
);
INSERT INTO iam_user (
    id, tenant_id, employee_no, display_name, legal_name, email,
    primary_org_id, manager_user_id, employment_status, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @wang_qiang_id, @tenant_id, CONCAT('EMP-', @wang_qiang_id), '王强', '王强', 'wang.qiang@example.invalid',
    NULL, NULL, 'EMPLOYED', 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_user WHERE tenant_id = @tenant_id AND email = 'wang.qiang@example.invalid'
);

SET @chen_chen_id := COALESCE(
    (SELECT id FROM iam_user WHERE tenant_id = @tenant_id AND email = 'chen.chen@example.invalid' LIMIT 1),
    '01KYDVHC00000000000000000F'
);
INSERT INTO iam_user (
    id, tenant_id, employee_no, display_name, legal_name, email,
    primary_org_id, manager_user_id, employment_status, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @chen_chen_id, @tenant_id, CONCAT('EMP-', @chen_chen_id), '陈晨', '陈晨', 'chen.chen@example.invalid',
    NULL, NULL, 'EMPLOYED', 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_user WHERE tenant_id = @tenant_id AND email = 'chen.chen@example.invalid'
);

SET @liu_yang_id := COALESCE(
    (SELECT id FROM iam_user WHERE tenant_id = @tenant_id AND email = 'liu.yang@example.invalid' LIMIT 1),
    '01KYDVHC00000000000000000G'
);
INSERT INTO iam_user (
    id, tenant_id, employee_no, display_name, legal_name, email,
    primary_org_id, manager_user_id, employment_status, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @liu_yang_id, @tenant_id, CONCAT('EMP-', @liu_yang_id), '刘洋', '刘洋', 'liu.yang@example.invalid',
    NULL, NULL, 'EMPLOYED', 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_user WHERE tenant_id = @tenant_id AND email = 'liu.yang@example.invalid'
);

-- Managers and organization leaders are only filled when currently empty, preserving later manual edits.
UPDATE iam_user
SET manager_user_id = @zhang_wei_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id IN (@li_na_id, @wang_qiang_id) AND manager_user_id IS NULL;

UPDATE iam_user
SET manager_user_id = @chen_chen_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id = @liu_yang_id AND manager_user_id IS NULL;

UPDATE iam_org_unit
SET leader_user_id = @zhang_wei_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id IN (@tech_center_id, @platform_rd_id, @ops_support_id) AND leader_user_id IS NULL;

UPDATE iam_org_unit
SET leader_user_id = @chen_chen_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id IN (@product_ops_center_id, @product_dept_id, @customer_success_id) AND leader_user_id IS NULL;

-- -----------------------------------------------------------------------------
-- Primary memberships
-- -----------------------------------------------------------------------------
SET @membership_id := '01KYDVHC00000000000000000H';
INSERT INTO iam_membership (
    id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @membership_id, @tenant_id, @zhang_wei_id, @platform_rd_id, @backend_position_id,
    'PRIMARY', 1, @seed_now, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_membership
    WHERE tenant_id = @tenant_id AND user_id = @zhang_wei_id
      AND org_unit_id = @platform_rd_id AND position_id = @backend_position_id
      AND membership_type = 'PRIMARY'
);

SET @membership_id := '01KYDVHC00000000000000000J';
INSERT INTO iam_membership (
    id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @membership_id, @tenant_id, @li_na_id, @platform_rd_id, @frontend_position_id,
    'PRIMARY', 1, @seed_now, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_membership
    WHERE tenant_id = @tenant_id AND user_id = @li_na_id
      AND org_unit_id = @platform_rd_id AND position_id = @frontend_position_id
      AND membership_type = 'PRIMARY'
);

SET @membership_id := '01KYDVHC00000000000000000K';
INSERT INTO iam_membership (
    id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @membership_id, @tenant_id, @wang_qiang_id, @ops_support_id, @ops_position_id,
    'PRIMARY', 1, @seed_now, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_membership
    WHERE tenant_id = @tenant_id AND user_id = @wang_qiang_id
      AND org_unit_id = @ops_support_id AND position_id = @ops_position_id
      AND membership_type = 'PRIMARY'
);

SET @membership_id := '01KYDVHC00000000000000000M';
INSERT INTO iam_membership (
    id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @membership_id, @tenant_id, @chen_chen_id, @product_dept_id, @product_position_id,
    'PRIMARY', 1, @seed_now, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_membership
    WHERE tenant_id = @tenant_id AND user_id = @chen_chen_id
      AND org_unit_id = @product_dept_id AND position_id = @product_position_id
      AND membership_type = 'PRIMARY'
);

SET @membership_id := '01KYDVHC00000000000000000N';
INSERT INTO iam_membership (
    id, tenant_id, user_id, org_unit_id, position_id, membership_type, is_primary,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @membership_id, @tenant_id, @liu_yang_id, @customer_success_id, @customer_success_position_id,
    'PRIMARY', 1, @seed_now, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM iam_membership
    WHERE tenant_id = @tenant_id AND user_id = @liu_yang_id
      AND org_unit_id = @customer_success_id AND position_id = @customer_success_position_id
      AND membership_type = 'PRIMARY'
);

-- Keep each test user's primary organization consistent with the primary membership.
UPDATE iam_user
SET primary_org_id = @platform_rd_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id IN (@zhang_wei_id, @li_na_id) AND primary_org_id IS NULL;

UPDATE iam_user
SET primary_org_id = @ops_support_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id = @wang_qiang_id AND primary_org_id IS NULL;

UPDATE iam_user
SET primary_org_id = @product_dept_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id = @chen_chen_id AND primary_org_id IS NULL;

UPDATE iam_user
SET primary_org_id = @customer_success_id, version = version + 1,
    updated_at = @seed_now, updated_by = @operator_user_id
WHERE id = @liu_yang_id AND primary_org_id IS NULL;

-- -----------------------------------------------------------------------------
-- Built-in ordinary-user role bindings. The role intentionally has no platform
-- management permissions and mirrors the automatic binding performed by POST /users.
-- -----------------------------------------------------------------------------
SET @new_binding_count := 0;

SET @binding_id := '01KYDVHC00000000000000000P';
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id,
    subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @binding_id, @tenant_id, @application_id, @ordinary_role_id,
    'USER', @zhang_wei_id, 'TENANT', '',
    NULL, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM authz_role_binding
    WHERE tenant_id = @tenant_id AND application_id = @application_id
      AND role_id = @ordinary_role_id AND subject_type = 'USER'
      AND subject_id = @zhang_wei_id AND scope_type = 'TENANT' AND scope_id = ''
);
SET @new_binding_count := @new_binding_count + ROW_COUNT();

SET @binding_id := '01KYDVHC00000000000000000Q';
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id,
    subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @binding_id, @tenant_id, @application_id, @ordinary_role_id,
    'USER', @li_na_id, 'TENANT', '',
    NULL, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM authz_role_binding
    WHERE tenant_id = @tenant_id AND application_id = @application_id
      AND role_id = @ordinary_role_id AND subject_type = 'USER'
      AND subject_id = @li_na_id AND scope_type = 'TENANT' AND scope_id = ''
);
SET @new_binding_count := @new_binding_count + ROW_COUNT();

SET @binding_id := '01KYDVHC00000000000000000R';
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id,
    subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @binding_id, @tenant_id, @application_id, @ordinary_role_id,
    'USER', @wang_qiang_id, 'TENANT', '',
    NULL, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM authz_role_binding
    WHERE tenant_id = @tenant_id AND application_id = @application_id
      AND role_id = @ordinary_role_id AND subject_type = 'USER'
      AND subject_id = @wang_qiang_id AND scope_type = 'TENANT' AND scope_id = ''
);
SET @new_binding_count := @new_binding_count + ROW_COUNT();

SET @binding_id := '01KYDVHC00000000000000000S';
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id,
    subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @binding_id, @tenant_id, @application_id, @ordinary_role_id,
    'USER', @chen_chen_id, 'TENANT', '',
    NULL, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM authz_role_binding
    WHERE tenant_id = @tenant_id AND application_id = @application_id
      AND role_id = @ordinary_role_id AND subject_type = 'USER'
      AND subject_id = @chen_chen_id AND scope_type = 'TENANT' AND scope_id = ''
);
SET @new_binding_count := @new_binding_count + ROW_COUNT();

SET @binding_id := '01KYDVHC00000000000000000T';
INSERT INTO authz_role_binding (
    id, tenant_id, application_id, role_id,
    subject_type, subject_id, scope_type, scope_id,
    valid_from, valid_until, status, version,
    created_at, created_by, updated_at, updated_by
)
SELECT
    @binding_id, @tenant_id, @application_id, @ordinary_role_id,
    'USER', @liu_yang_id, 'TENANT', '',
    NULL, NULL, 'ACTIVE', 1,
    @seed_now, @operator_user_id, @seed_now, @operator_user_id
WHERE NOT EXISTS (
    SELECT 1 FROM authz_role_binding
    WHERE tenant_id = @tenant_id AND application_id = @application_id
      AND role_id = @ordinary_role_id AND subject_type = 'USER'
      AND subject_id = @liu_yang_id AND scope_type = 'TENANT' AND scope_id = ''
);
SET @new_binding_count := @new_binding_count + ROW_COUNT();

UPDATE authz_policy_revision
SET revision = revision + 1,
    changed_at = @seed_now,
    change_reason = '本地测试用户绑定普通用户角色'
WHERE tenant_id = @tenant_id
  AND application_id = @application_id
  AND @new_binding_count > 0;

COMMIT;

-- Return a small verification result to the caller.
SELECT 'test_users' AS category, COUNT(*) AS row_count
FROM iam_user
WHERE tenant_id = @tenant_id
  AND email IN (
      'zhang.wei@example.invalid',
      'li.na@example.invalid',
      'wang.qiang@example.invalid',
      'chen.chen@example.invalid',
      'liu.yang@example.invalid'
  )
UNION ALL
SELECT 'test_org_units', COUNT(*)
FROM iam_org_unit
WHERE tenant_id = @tenant_id
  AND code IN (
      'TECH_CENTER', 'PLATFORM_RD', 'OPS_SUPPORT',
      'PRODUCT_OPS_CENTER', 'PRODUCT_DEPT', 'CUSTOMER_SUCCESS'
  )
UNION ALL
SELECT 'test_positions', COUNT(*)
FROM iam_position
WHERE tenant_id = @tenant_id
  AND code IN (
      'BACKEND_ENGINEER', 'FRONTEND_ENGINEER', 'OPS_ENGINEER',
      'PRODUCT_MANAGER', 'CUSTOMER_SUCCESS_SPECIALIST'
  )
UNION ALL
SELECT 'test_memberships', COUNT(*)
FROM iam_membership m
JOIN iam_user u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id
  AND u.email LIKE '%@example.invalid'
UNION ALL
SELECT 'ordinary_role_bindings', COUNT(*)
FROM authz_role_binding rb
JOIN iam_user u ON u.id = rb.subject_id
WHERE rb.tenant_id = @tenant_id
  AND rb.application_id = @application_id
  AND rb.role_id = @ordinary_role_id
  AND rb.subject_type = 'USER'
  AND u.email LIKE '%@example.invalid';
