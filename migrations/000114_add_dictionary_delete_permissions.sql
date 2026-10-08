-- Adds the high-risk permissions backing the physical-delete dictionary endpoints.
-- Deleting a dictionary cascades to its items inside one transaction, so the delete
-- grant is separate from update and must not be implied by it.

INSERT INTO authz_permission (id, tenant_id, application_id, resource_id, code, action, name, description, risk_level, status, version, created_at, updated_at)
VALUES
    ('01J00000000000000000000523', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000122', 'platform:dictionary:delete', 'delete', '删除业务字典', '物理删除业务字典，并级联删除其全部字典项；该操作不可恢复。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
    ('01J00000000000000000000524', '01J00000000000000000000000', '01J00000000000000000000001', '01J00000000000000000000123', 'platform:dictionary-item:delete', 'delete', '删除业务字典项', '物理删除单个字典项；已发布到业务的值建议停用而非删除。', 'HIGH', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    risk_level = VALUES(risk_level),
    status = VALUES(status),
    updated_at = UTC_TIMESTAMP(3);

INSERT IGNORE INTO authz_role_permission (role_id, permission_id, effect, created_at)
SELECT role.id, permission.id, 'ALLOW', UTC_TIMESTAMP(3)
FROM authz_role AS role
JOIN authz_permission AS permission
    ON permission.tenant_id = role.tenant_id
    AND permission.application_id = role.application_id
WHERE role.tenant_id = '01J00000000000000000000000'
  AND role.application_id = '01J00000000000000000000001'
  AND role.code IN ('platform-super-admin', 'platform-security-admin')
  AND permission.code IN (
      'platform:dictionary:delete',
      'platform:dictionary-item:delete'
  );

-- Seeds the first business-consumed dictionary: personnel change types. The item_value
-- set is a backend workflow protocol (PersonnelChangeCenter branches on it), so the
-- dictionary governs display names and ordering only; admins must not invent new values.
INSERT INTO dict_dictionary (id, tenant_id, code, name, description, status, version, created_at, created_by, updated_at, updated_by)
VALUES ('01J00000000000000000000530', '01J00000000000000000000000', 'PERSONNEL_CHANGE_TYPE', '人员异动类型', '人员异动中心的异动类型显示名；item_value 为后端流程协议值，仅允许调整显示名称、排序与启停，不得新增或删除取值。', 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    description = VALUES(description),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO dict_item (id, tenant_id, dictionary_id, code, label, item_value, sort_order, status, version, created_at, created_by, updated_at, updated_by)
VALUES
    ('01J00000000000000000000531', '01J00000000000000000000000', '01J00000000000000000000530', 'PROMOTION', '晋升', 'PROMOTION', 1, 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000532', '01J00000000000000000000000', '01J00000000000000000000530', 'DEMOTION', '降职', 'DEMOTION', 2, 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000533', '01J00000000000000000000000', '01J00000000000000000000530', 'TRANSFER', '调岗', 'TRANSFER', 3, 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000534', '01J00000000000000000000000', '01J00000000000000000000530', 'TERMINATION', '离职', 'TERMINATION', 4, 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000535', '01J00000000000000000000000', '01J00000000000000000000530', 'REHIRE', '复职', 'REHIRE', 5, 'ACTIVE', 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE
    label = VALUES(label),
    item_value = VALUES(item_value),
    sort_order = VALUES(sort_order),
    updated_at = UTC_TIMESTAMP(3);

INSERT INTO authz_policy_revision (tenant_id, application_id, revision, changed_at, change_reason)
VALUES ('01J00000000000000000000000', '01J00000000000000000000001', 16, UTC_TIMESTAMP(3), '新增业务字典删除权限')
ON DUPLICATE KEY UPDATE
    revision = GREATEST(revision, 16),
    changed_at = UTC_TIMESTAMP(3),
    change_reason = '新增业务字典删除权限';
