-- Extends migration-owned storage for public async job identifiers and configuration display names.
-- No GORM AutoMigrate is used by this project.
ALTER TABLE async_job
    ADD COLUMN public_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER id,
    ADD UNIQUE KEY uk_async_job_public_id (public_id);

ALTER TABLE cfg_namespace
    ADD COLUMN display_name VARCHAR(100) NULL AFTER name;

UPDATE cfg_namespace
SET display_name = name
WHERE display_name IS NULL;

ALTER TABLE cfg_namespace
    MODIFY COLUMN display_name VARCHAR(100) NOT NULL;
