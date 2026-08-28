CREATE TABLE IF NOT EXISTS file_gateway_config (
    id TINYINT UNSIGNED NOT NULL,
    storage_backend VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    endpoint VARCHAR(512) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT chk_file_gateway_config_backend CHECK (storage_backend IN ('LOCAL', 'OBJECT'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
