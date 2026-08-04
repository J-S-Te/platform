-- 子系统接入生命周期站内通知模板。
-- 平台在子系统接入/重试成功或失败时，自动给操作人发送站内通知；
-- 子系统代码无需改动，模板与投递全部由基础平台通知中心完成。
INSERT INTO notification_template (id, tenant_id, code, name, status, current_version, version, created_at, created_by, updated_at, updated_by)
VALUES
    ('01J00000000000000000000501', '01J00000000000000000000000', 'subsystem.lifecycle.succeeded', '子系统接入成功', 'ACTIVE', 1, 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000502', '01J00000000000000000000000', 'subsystem.lifecycle.failed', '子系统接入失败', 'ACTIVE', 1, 1, UTC_TIMESTAMP(3), NULL, UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE id = id;

INSERT INTO notification_template_version (id, template_id, tenant_id, version, status, title_template, body_template, variables_json, published_at, created_at, created_by)
VALUES
    ('01J00000000000000000000511', '01J00000000000000000000501', '01J00000000000000000000000', 1, 'PUBLISHED',
     '子系统接入成功',
     '{{application_name}}（{{application_code}}/{{environment}}）接入成功，可以开始使用。',
     '[{"name":"application_name","required":true,"max_length":128},{"name":"application_code","required":true,"max_length":64},{"name":"environment","required":true,"max_length":16}]',
     UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), NULL),
    ('01J00000000000000000000512', '01J00000000000000000000502', '01J00000000000000000000000', 1, 'PUBLISHED',
     '子系统接入失败',
     '{{application_name}}（{{application_code}}/{{environment}}）接入失败：{{detail}}',
     '[{"name":"application_name","required":true,"max_length":128},{"name":"application_code","required":true,"max_length":64},{"name":"environment","required":true,"max_length":16},{"name":"detail","required":false,"max_length":500}]',
     UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE id = id;
