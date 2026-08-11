-- 为跨应用审计事件补充业务关联标识，用于关联同一业务链路中的多个请求与追踪。
ALTER TABLE audit_event
    ADD COLUMN correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER trace_id,
    ADD KEY idx_audit_event_correlation_occurred (correlation_id, occurred_at);
