-- Explicit controlled-release replacement never grants migration eligibility.
-- Tombstones retain component and OAuth identities to reject stale machine tokens.
ALTER TABLE license_runtime_member ADD COLUMN retired_at DATETIME(3) NULL;
CREATE TABLE license_runtime_lifecycle_event (
 operation_id VARCHAR(128) NOT NULL PRIMARY KEY,
 application VARCHAR(128) NOT NULL,
 request_digest VARCHAR(64) NOT NULL,
 release_digest VARCHAR(71) NOT NULL,
 old_specs MEDIUMTEXT NOT NULL,
 new_specs MEDIUMTEXT NOT NULL,
 revision BIGINT UNSIGNED NOT NULL,
 tenant_id VARCHAR(128) NOT NULL,
 user_id VARCHAR(128) NOT NULL,
 created_at DATETIME(3) NOT NULL,
 KEY ix_license_runtime_lifecycle_application (application,revision)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE license_runtime_retired_client (
 oauth_client_id VARCHAR(128) NOT NULL PRIMARY KEY,
 service_id VARCHAR(128) NOT NULL,
 retired_at DATETIME(3) NOT NULL,
 KEY ix_license_runtime_retired_service (service_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
