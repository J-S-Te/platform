-- A terminal reference marker survives response loss and rejects late Bind.
-- No file FK: v2 may allocate a file_id before a physical upload starts.
-- The service verifies ownership through file_object OR the exact v2 session.
CREATE TABLE IF NOT EXISTS file_binding_release (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    file_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    binding_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_file_binding_release (tenant_id,application_id,file_id,resource_type,resource_id,binding_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
