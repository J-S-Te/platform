-- 统一审计方案 §2.3：登录地址来源独立标记。
--
-- 背景：ip_source 描述“当前操作请求”的来源，不能用来推断历史登录地址的来源。
-- 登录地址与会话绑定，其来源必须在会话创建时单独记录；历史行保持 NULL，
-- 界面显示“历史记录无此字段”，不推算、不回填。
ALTER TABLE audit_event
    ADD COLUMN login_ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER ip_source;
