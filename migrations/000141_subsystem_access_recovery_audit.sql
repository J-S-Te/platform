-- 子系统接入：原代次被取代的人工恢复审计（000141）。
--
-- 背景：一次受控更新派发后结果未知（DEPLOYMENT_EFFECT_UNKNOWN）时，Agent 的人工恢复
-- 栅栏要求 `request.Receipt.ManifestChecksum == 当前 profile 校验和`。CI 发布流程随后
-- 直接部署该子系统并替换了 profile 清单（descriptor/模板变化），这个等式永久不成立，
-- 而看护任务又因 `NOT EXISTS (DISPATCHED attempt)` 守卫跳过该代次，最终
-- subsystem_deployment_state 永久停在 UPDATING，拒绝一切受控操作。
--
-- 关键事实：Agent 的两个否定证明——没有成功回执（readAccessDeploymentReceipt）与没有
-- 终止结果（readAccessDeploymentOutcome）——只依赖回执散列路径，不依赖当前 profile
-- 清单。因此清单漂移后仍可证明"确实没成功、也没终止"。这条路径只读取证明、只做失败
-- 收口：既不声明部署成功，也不自动回滚，更不修改 generation（代次只能由 Claim 递增）。
--
-- 本表记录操作者实际审核到的审核基准与收口前的部署状态快照，用于事后说明该收口判断
-- 的依据。applied/desired 清单摘要原值为 NULL 时统一落空串，避免把"未知"写成某种摘要。
--
-- 只追加：数据库层用触发器拒绝 UPDATE 与 DELETE（同 000130 的审计只追加约束）。
-- 迁移执行器逐语句执行、不支持 DELIMITER，因此触发器体保持单条语句。
CREATE TABLE IF NOT EXISTS subsystem_access_recovery_audit (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id CHAR(26) NOT NULL,
    application_code VARCHAR(64) NOT NULL,
    environment_code VARCHAR(16) NOT NULL,
    operation_id CHAR(26) NOT NULL,
    generation BIGINT UNSIGNED NOT NULL,
    receipt_manifest_checksum VARCHAR(128) NOT NULL,
    current_profile_checksum VARCHAR(128) NOT NULL,
    state_status VARCHAR(32) NOT NULL,
    state_generation BIGINT UNSIGNED NOT NULL,
    state_desired_manifest_checksum VARCHAR(128) NOT NULL DEFAULT '',
    state_applied_manifest_checksum VARCHAR(128) NOT NULL DEFAULT '',
    operator_id CHAR(26) NOT NULL,
    reason VARCHAR(500) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_subsystem_access_recovery_audit_operation (tenant_id, operation_id),
    KEY idx_subsystem_access_recovery_audit_target (tenant_id, application_code, environment_code, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TRIGGER subsystem_access_recovery_audit_no_update
  BEFORE UPDATE ON subsystem_access_recovery_audit
  FOR EACH ROW
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'subsystem_access_recovery_audit is append-only: UPDATE rejected';

CREATE TRIGGER subsystem_access_recovery_audit_no_delete
  BEFORE DELETE ON subsystem_access_recovery_audit
  FOR EACH ROW
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'subsystem_access_recovery_audit is append-only: DELETE rejected';
