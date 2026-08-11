-- Preserve application-owned opaque Claims compatibility metadata with the synchronized authorization catalog.
-- The base platform must not derive subsystem-specific role/permission hashes.
ALTER TABLE authz_authorization_catalog
    ADD COLUMN claims_role_config_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''
    AFTER catalog_hash;
