-- Preserve pre-upgrade installation facts separately from target image membership.
CREATE TABLE license_installation_baseline (
 id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
 scenario VARCHAR(32) NOT NULL,
 project VARCHAR(128) NOT NULL,
 instance_id VARCHAR(128) NOT NULL,
 environment VARCHAR(128) NOT NULL,
 evidence_digest VARCHAR(64) NOT NULL,
 evidence_json MEDIUMTEXT NOT NULL,
 collected_at DATETIME(3) NOT NULL,
 created_at DATETIME(3) NOT NULL,
 tenant_id VARCHAR(128) NOT NULL,
 user_id VARCHAR(128) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
