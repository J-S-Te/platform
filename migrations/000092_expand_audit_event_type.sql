-- 审计事件类型可能包含带路径的 HTTP 路由，例如 HTTP_GET /customer-opportunity/api/v1/...
-- 64 个字符不足以容纳这类事件，导致审计写入失败并产生 Error 1406。
ALTER TABLE audit_event
    MODIFY COLUMN event_type VARCHAR(128) NOT NULL;
