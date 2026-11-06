-- 审计导出下载的一次性票据（审计导出下载契约变更）。
--
-- 背景：GET /api/v1/audit/export-jobs/{jobId}/download 原先只凭会话 Cookie + 权限即可
-- 重复、并发下载同一份导出产物，链接被复制或代理缓存后可被无限次重新获取。改造后下载
-- 分两步：先申请短时一次性票据，再用票据兑换文件字节；票据一次性、原子核销，并发兑换
-- 最多一个成功。
--
-- 本表只保存票据的 SHA-256 摘要：明文 token 只在签发响应里出现一次，绝不落库、不入
-- 审计载荷、不入日志。过期或已使用的行不提供明文恢复能力，因此泄露本表也不能兑换文件。
--
-- 兑换语义（audit.Repository.ConsumeExportDownloadTicket）：
--   单条 UPDATE ... WHERE tenant_id = ? AND job_id = ? AND token_hash = ?
--        AND used_at IS NULL AND expires_at > ?
--   只有命中未使用且未过期行的调用方 RowsAffected = 1，才允许输出文件；因此并发兑换
--   最多一个成功（另一方看到 used_at 已写或条件不成立，按“无效/已用”拒绝）。
--
-- 清理：票据 TTL 只有 2 分钟，签发新票据时会顺带删除同一租户同一任务 1 小时前的行，
-- 票据表不会随下载次数无界增长；不提供自动逆向迁移（删除本表会丢掉“谁领过票据”的
-- 唯一证据，需要清理时由运维手工执行 DROP TABLE audit_export_download_ticket）。
--
-- job_id 是 async_job.public_id 的公开标识（CHAR(26) ULID），故意不加外键：
-- 票据是短命 bearer 记录，兑换查询本身按（tenant_id, job_id, token_hash）限定范围，
-- 不希望对 async_job 的行保留/清理策略产生级联约束。
CREATE TABLE IF NOT EXISTS audit_export_download_ticket (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    job_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_hash BINARY(32) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    used_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_audit_export_download_ticket_hash (token_hash),
    KEY idx_audit_export_download_ticket_job (tenant_id, job_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
