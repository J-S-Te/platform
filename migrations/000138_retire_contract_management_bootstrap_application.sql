-- 合同管理不再由平台迁移内嵌注册：删除 000045 写入、且从未被真实接入使用过的引导行。
--
-- 背景：000045 用固定 ID 在平台库里预置了 contract_management 的应用、dev 环境和 home
-- 登录目标（权限当时还挂在平台应用下，000059 才迁到通用授权模型）。这套预置让合同成为
-- 唯一"内嵌注册"的子系统：应用记录天然存在，无法作为新的 Docker discovery 候选走
-- 与其他五个子系统一致的"探测 → 应用接入"路径。000111 已退役并（在无使用证据时）删除
-- 了预置 dev 环境；本迁移在同一安全边界下移除剩余的引导应用行，使全新初始化的平台库
-- 里合同管理只能通过 discovery 候选 + 应用接入创建。
--
-- 历史迁移不可改写（000045 已被 embed_test 的校验和锁定），因此这里用新增迁移表达"退役"。
--
-- 安全边界（必须同时成立才删除，任一不成立即保持原样、不写入任何变更）：
--   * 仍是 000045 写入的固定 ID / 固定租户，且 created_by 为空（引导写入而非管理员接入）；
--   * 没有任何环境、登录目标、OAuth 客户端、部署状态、服务实例；
--   * 没有配置命名空间、审计、文件、异步任务证据；
--   * 没有 Keycloak 映射/投影/切换证据；
--   * 没有授权目录、策略、资源、权限、角色、绑定或用户授权证据。
-- 已经真实接入过（生产把 prod 环境挂在引导应用上，或本地/测试库已手工接入 dev）的部署
-- 不满足上述条件，因此不会被本迁移修改，既有 OAuth 客户端、绑定与运行配置全部保留。
--
-- 回滚：删除的只是无任何引用证据的引导行；代码回滚后 000045 会在全新库重新写入同样的
-- 引导行，因此不提供逆向迁移（避免与"合同已重新接入"的新记录冲突）。
DELETE application
FROM platform_application AS application
WHERE application.id = '01J00000000000000000000300'
  AND application.code = 'contract_management'
  AND application.tenant_id = '01J00000000000000000000000'
  AND application.created_by IS NULL
  AND NOT EXISTS (SELECT 1 FROM platform_application_environment AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM platform_application_login_target AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM platform_oauth_client AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM subsystem_deployment_state AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM subsystem_service_instance AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM cfg_namespace AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM audit_archive AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM audit_ingestion_receipt AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM audit_retention_task AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM async_job AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM file_binding AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM file_object AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_application_authorization_policy AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_authorization_catalog AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_policy_revision AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_resource AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_permission AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_role AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_role_binding AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_user_application_role AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_user_permission AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM authz_position_grant_template_role AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_application_client_mapping AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_authorization_outbox AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_authorization_projection AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_broker_login_verification AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_cutover_lifecycle AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_cutover_timeline_event AS item WHERE item.application_id = application.id)
  AND NOT EXISTS (SELECT 1 FROM keycloak_switch_readiness AS item WHERE item.application_id = application.id);
