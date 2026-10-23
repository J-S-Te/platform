-- 统一审计来源协议：把三类地址拆成独立列，禁止互相冒充。
--
-- login_ip        创建应用会话时观察到的客户端地址（登录 / OIDC 回调时固定）
-- client_ip       本次业务操作的客户端地址（每个请求重新解析）
-- ingest_peer_ip  向平台投递审计的服务或 Worker 地址（平台摄取端记录）
-- ip_source       地址来源标记：DIRECT / TRUSTED_PROXY / LEGACY_PROTOCOL / UNKNOWN
--
-- 历史行保留原 source_ip 原值并把 ip_source 记为 LEGACY_RECORD，不做猜测性回填。
ALTER TABLE audit_event
    ADD COLUMN login_ip VARBINARY(16) NULL AFTER source_ip,
    ADD COLUMN client_ip VARBINARY(16) NULL AFTER login_ip,
    ADD COLUMN ingest_peer_ip VARBINARY(16) NULL AFTER client_ip,
    ADD COLUMN ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LEGACY_RECORD' AFTER ingest_peer_ip,
    ADD KEY idx_audit_event_login_ip (tenant_id, login_ip, occurred_at),
    ADD KEY idx_audit_event_client_ip (tenant_id, client_ip, occurred_at);
