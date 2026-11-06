-- 为权限增加授予策略元数据（子系统 CRM 首次发布 customer.read.all / customer_all_viewer）。
--
-- grant_policy 描述"该权限所属角色可以由谁授予"：
--   * DEFAULT          —— 现有语义，按角色绑定与统一委派规则授予；
--   * SUPER_ADMIN_ONLY —— 该权限所属角色只能由租户级平台超级管理员
--                         （platform-super-admin）授予；平台安全管理员豁免与
--                         普通操作者的"只能委派本人已有权限"检查都不得绕过它。
--
-- 存量与内置权限全部保持 DEFAULT，行为不变；不需要回填，也没有历史数据可以据此
-- 反推更高限制。列类型与同表 risk_level 保持一致（同样不额外声明字符集/排序规则，
-- 沿用表的 utf8mb4_unicode_ci），长度按最长枚举值 SUPER_ADMIN_ONLY 留出余量。
--
-- 回滚：代码回滚后旧实现完全不读写本列；需要彻底清理时由运维手工执行
--   ALTER TABLE authz_permission DROP COLUMN grant_policy;
-- （有意不提供自动执行的逆向迁移：它会一并丢掉已备案的授予限制。）
ALTER TABLE authz_permission
    ADD COLUMN grant_policy VARCHAR(32) NOT NULL DEFAULT 'DEFAULT' AFTER risk_level;
