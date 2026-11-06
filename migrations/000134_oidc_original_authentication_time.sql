-- 旧授权码无可信起点时不补造时间，由正常重新登录生成新码。
ALTER TABLE oauth_authorization_code
    ADD COLUMN authenticated_at DATETIME(3) NULL AFTER created_at;
