-- Keycloak 授权投影 outbox 的派生聚合索引（000138）。
--
-- 背景：控制台“应用目录/子系统门户”按
--   identity_id = ? AND environment_id IS NOT NULL AND status IN ('PENDING','RUNNING','FAILED')
-- 从 keycloak_authorization_outbox 派生当前用户在各应用/环境的待处理队列计数
-- （internal/platform/applicationregistry/infrastructure/subsystem_onboarding_gorm_repository.go
-- 中的 projection_queue 子查询）。既有二级索引都以 tenant_id 或 status 打头：
--   * idx_keycloak_outbox_poll (status, available_at, created_at)
--   * idx_keycloak_outbox_subject (tenant_id, identity_id, application_id, status)
--   * idx_keycloak_outbox_environment_subject (tenant_id, identity_id, application_id, environment_id, status)
-- 该聚合谓词没有 tenant_id 等值条件，前两条以 tenant_id 打头的索引无法定位，只能退化为
-- 全表/全二级索引扫描；门户页每次打开都会执行它，outbox 越长越慢。
--
-- 索引选择：以 identity_id 打头的 (identity_id, status, environment_id)。
--   * 不选 (tenant_id, identity_id, status, environment_id)：谓词里没有 tenant_id 等值条件，
--     前置列会让索引对这条查询不可用（这正是现状的成因），因此重复该形状没有收益。
--     identity_id 是 iam_user 的主键 ID（ULID，全库唯一），单用户等值已经把范围收敛到很少的行，
--     不需要再用 tenant_id 缩小。
--   * status 放在第二位承接 IN 列表（索引范围），environment_id 放末位让 IS NOT NULL 通过
--     索引条件下推在索引内完成，无需回表即可丢弃 environment_id 为空的历史事件
--     （000080 有意保留旧事件的 NULL environment_id）。
--   * 不额外包含 application_id/tenant_id 做覆盖索引：单用户命中的行数个位数，
--     行查找代价可忽略，避免为一个观测型子查询加宽索引、放大写入成本。
--
-- 本迁移只加索引，不回填、不改写任何业务行；用 information_schema 守卫（同 000093/000133），
-- 使 DDL 失败或文件被重放时不会因索引已存在而中断。
SET @migration_sql := IF(
    EXISTS (
        SELECT 1 FROM information_schema.statistics
         WHERE table_schema = DATABASE()
           AND table_name = 'keycloak_authorization_outbox'
           AND index_name = 'idx_keycloak_outbox_identity_status'
    ),
    'SELECT 1',
    'ALTER TABLE keycloak_authorization_outbox ADD KEY idx_keycloak_outbox_identity_status (identity_id, status, environment_id)'
);
PREPARE migration_stmt FROM @migration_sql;
EXECUTE migration_stmt;
DEALLOCATE PREPARE migration_stmt;
