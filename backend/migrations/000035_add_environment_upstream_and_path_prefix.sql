-- 让统一登录门户变成单端口多路径的入口网关。
-- 给 application environment 增加两个解耦字段：
--   upstream_url: 子系真实监听的内部地址（仅在部署机内部可访问，例如 http://127.0.0.1:8081）
--   path_prefix:   子系在门户统一入口下的对外路径前缀（例如 /apps/contract）
-- BaseURL 现有语义保留为「该环境下子系对外可访问的完整基础 URL」，用于拼接 LoginTarget.TargetURI；
-- 当 TargetURI 改为相对路径模式时，解析时会基于 BaseURL + TargetURI 生成最终跳转地址。
ALTER TABLE platform_application_environment
    ADD COLUMN upstream_url VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER base_url,
    ADD COLUMN path_prefix VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER upstream_url,
    ADD KEY idx_app_environment_path_prefix (path_prefix);
