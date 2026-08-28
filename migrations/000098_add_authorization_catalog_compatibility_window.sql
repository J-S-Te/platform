-- 授权目录只保留当前版本与紧邻上一版本，形成受控的 N/N-1 滚动发布窗口。
-- 这里保存的是兼容元数据，不复制已经失效的角色绑定或权限关系。
ALTER TABLE authz_authorization_catalog
    ADD COLUMN previous_catalog_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''
        AFTER claims_role_config_hash,
    ADD COLUMN previous_catalog_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''
        AFTER previous_catalog_version,
    ADD COLUMN previous_claims_role_config_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''
        AFTER previous_catalog_hash;
