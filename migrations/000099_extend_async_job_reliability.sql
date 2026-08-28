-- 补齐通用异步任务的幂等、重跑谱系、重试累计和可观测性字段。
-- 所有新增字段允许历史记录继续读取；只有新任务强制写入 request_hash。
ALTER TABLE async_job
    ADD COLUMN parent_job_id BIGINT UNSIGNED NULL AFTER public_id,
	ADD COLUMN application_idempotency_scope VARCHAR(26) CHARACTER SET ascii COLLATE ascii_bin
		GENERATED ALWAYS AS (IFNULL(application_id, '')) STORED AFTER application_id,
    ADD COLUMN idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER aggregate_id,
    ADD COLUMN request_hash BINARY(32) NULL AFTER payload,
    ADD COLUMN request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER request_hash,
    ADD COLUMN trace_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER request_id,
    ADD COLUMN correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER trace_id,
    ADD COLUMN business_ref VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL AFTER correlation_id,
    ADD COLUMN last_attempt_at DATETIME(3) NULL AFTER locked_at,
    ADD COLUMN retry_count INT UNSIGNED NOT NULL DEFAULT 0 AFTER attempts,
    ADD COLUMN last_succeeded_at DATETIME(3) NULL AFTER completed_at,
    ADD UNIQUE KEY uk_async_job_idempotency (tenant_id, application_idempotency_scope, job_type, idempotency_key),
    ADD KEY idx_async_job_parent (parent_job_id),
    ADD KEY idx_async_job_last_success (last_succeeded_at),
    ADD KEY idx_async_job_correlation (correlation_id, created_at),
    ADD CONSTRAINT fk_async_job_parent FOREIGN KEY (parent_job_id) REFERENCES async_job (id) ON DELETE RESTRICT;
