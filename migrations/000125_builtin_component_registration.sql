-- Persist the registration mode separately from the signed license/snapshot
-- protocol. Existing rows remain strict V1; only a controlled coordination
-- change may move an application to the V2 bridge/builtin modes.
ALTER TABLE license_runtime_application
  ADD COLUMN registration_mode VARCHAR(32) NOT NULL DEFAULT 'LEGACY_IMAGE_V1' AFTER state,
  ADD COLUMN catalog_version INT UNSIGNED NOT NULL DEFAULT 0 AFTER registration_mode;

ALTER TABLE license_runtime_member
  ADD COLUMN registration_protocol INT UNSIGNED NOT NULL DEFAULT 1 AFTER oauth_client_id,
  ADD COLUMN catalog_version INT UNSIGNED NOT NULL DEFAULT 0 AFTER registration_protocol,
  ADD COLUMN credential_generation VARCHAR(128) NOT NULL DEFAULT '' AFTER catalog_version;
