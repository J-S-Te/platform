-- 安全（SEC-D2）：cfg_namespace.tenant_id 必须与所绑定应用的 tenant_id 一致。
-- platform_application 只在 (tenant_id, code) 上唯一，此前按 code 查应用不带租户过滤，
-- 租户 A 可把配置命名空间绑到租户 B 的同码应用，留下 FK RESTRICT 行阻塞租户 B 删除环境。
-- MySQL 复合外键要求被引用侧存在列相同的唯一索引：(id, tenant_id) 因 id 是主键天然唯一，
-- 在此显式建出索引供复合 FK 引用。
--
-- fail-closed 说明：ALTER TABLE ADD FOREIGN KEY 会校验存量数据；若库里已经存在跨租户
-- 绑定的脏行，本迁移会直接失败并中止升级——这是有意的 guard，必须先人工修复脏数据
-- （把 cfg_namespace.tenant_id 更正为所绑定应用的租户，或删除该命名空间）再重跑迁移。

ALTER TABLE platform_application ADD UNIQUE KEY uk_platform_application_id_tenant (id, tenant_id);

ALTER TABLE cfg_namespace DROP FOREIGN KEY fk_cfg_namespace_application;

ALTER TABLE cfg_namespace
    ADD CONSTRAINT fk_cfg_namespace_application
    FOREIGN KEY (application_id, tenant_id)
    REFERENCES platform_application (id, tenant_id)
    ON DELETE RESTRICT;
