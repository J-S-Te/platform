-- 让统一登录门户变成单端口多路径的入口网关。
-- 给 application environment 增加两个解耦字段：
--   upstream_url: 子系真实监听的内部地址（仅在部署机内部可访问，例如 http://127.0.0.1:8081）
--   path_prefix:   子系在门户统一入口下的对外路径前缀（例如 /contract）
-- BaseURL 表示所有浏览器可访问的门户统一地址（例如 http://portal-ip），不再承载内部监听地址；
-- 当 TargetURI 为相对路径时，解析时基于 BaseURL + PathPrefix + TargetURI 生成最终跳转地址。
ALTER TABLE platform_application_environment
    ADD COLUMN upstream_url VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER base_url,
    ADD COLUMN path_prefix VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER upstream_url,
    ADD KEY idx_app_environment_path_prefix (path_prefix);
