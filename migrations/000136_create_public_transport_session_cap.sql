-- HTTPS→HTTP 降级时被截断的会话有效期原始值（BIZ-RUNTIME-06）。
--
-- 背景：降级在 Begin 阶段把 iam_session / oauth_token_family / oauth_refresh_token 中
-- expires_at 超过排空截止时间的行直接改写到截止时间，原值不保存。于是一次失败或被运维
-- 放弃的降级仍然把全平台会话永久缩短——"没切成"却已经强制所有人重新登录。
--
-- 本表只保存恢复所需的非秘密元数据：切换 ID、表名、行 ID 和两个到期时间。
-- 不保存任何会话内容、Token、Cookie、摘要或租户/账号标识。
--
-- 写入语义（publictransport.capSessionLifetimes）：
--   INSERT IGNORE ... SELECT（唯一键即主键）⇒ 重试幂等，已记录过的行不会重复记录，
--   也不会被二次截断（UPDATE 只覆盖 expires_at 仍大于 capped_expires_at 的行）。
-- 恢复语义（publictransport.restoreSessionLifetimes）：
--   UPDATE ... JOIN 只在"当前值仍等于截断值"时复原，避免覆盖截断之后发生的合法变更；
--   复原成功的行随即删除，部分未被复原的行保留为可审计证据。
--
-- 回填：不做。历史切换没有原始到期时间可用，无法凭空还原；本文档与 OFFLINE_RUNBOOK
-- 已注明"升级前的降级切换不可恢复"。
-- 回滚：代码回滚后旧实现完全不读本表；需要彻底清理时由运维手工执行
--   DROP TABLE platform_public_transport_session_cap;
-- （有意不提供自动执行的逆向迁移：它会把审计证据一并删掉。）
CREATE TABLE IF NOT EXISTS platform_public_transport_session_cap (
    transition_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_table ENUM('iam_session', 'oauth_token_family', 'oauth_refresh_token') CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_expires_at DATETIME(3) NOT NULL,
    capped_expires_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (transition_id, session_table, session_id),
    KEY idx_public_transport_session_cap_session (session_table, session_id),
    CONSTRAINT chk_public_transport_session_cap_order CHECK (original_expires_at >= capped_expires_at),
    CONSTRAINT fk_public_transport_session_cap_transition FOREIGN KEY (transition_id) REFERENCES platform_public_transport_transition (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
