-- Personnel lifecycle notification template. Recipients are resolved at delivery time
-- from the user, operator, and target organization supplied by the change event.
INSERT INTO notification_template (id, tenant_id, code, name, status, current_version, version, created_at, created_by, updated_at, updated_by)
VALUES ('01J00000000000000000000521', '01J00000000000000000000000', 'personnel_change_executed', '人员异动已生效', 'ACTIVE', 1, 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO notification_template_version (id, template_id, tenant_id, version, status, title_template, body_template, variables_json, published_at, created_at, created_by)
VALUES ('01J00000000000000000000522', '01J00000000000000000000521', '01J00000000000000000000000', 1, 'PUBLISHED',
        '人员异动已生效',
        '人员异动单 {{request_id}} 已生效，类型：{{change_type}}。原因：{{reason}}',
        '[{"name":"request_id","required":true,"max_length":64},{"name":"change_type","required":true,"max_length":32},{"name":"reason","required":true,"max_length":500}]',
        UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE id = id;
