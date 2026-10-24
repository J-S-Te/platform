-- No transition eligibility is seeded. Only trusted installation evidence can
-- freeze the original inventory; newly registered systems remain fail-closed.
CREATE TABLE IF NOT EXISTS license_runtime_inventory (
 id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
 project VARCHAR(128) NOT NULL,
 evidence_digest VARCHAR(64) NOT NULL,
 collected_at DATETIME(3) NOT NULL,
 created_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS license_runtime_application (
 application VARCHAR(128) NOT NULL PRIMARY KEY,
 state VARCHAR(32) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 migration_eligible BOOLEAN NOT NULL DEFAULT FALSE,
 content_digest VARCHAR(64) NOT NULL DEFAULT '',
 updated_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS license_runtime_member (
 service_id VARCHAR(128) NOT NULL PRIMARY KEY,
 application VARCHAR(128) NOT NULL,
 environment VARCHAR(128) NOT NULL,
 oauth_client_id VARCHAR(128) NOT NULL,
 coverage_digest VARCHAR(71) NOT NULL,
 image_digest VARCHAR(71) NOT NULL,
 ready_at DATETIME(3) NULL,
 issued_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
 issued_digest VARCHAR(64) NOT NULL DEFAULT '',
 raw_jws MEDIUMTEXT NOT NULL,
 ack_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ack_at DATETIME(3) NULL,
 created_at DATETIME(3) NOT NULL,
 KEY ix_license_runtime_application (application,service_id),
 UNIQUE KEY uq_license_runtime_client (oauth_client_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
