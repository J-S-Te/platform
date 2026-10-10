-- Commercial signatures remain the authority. No business system is enforced
-- by this migration and no existing deployment receives transition eligibility.
CREATE TABLE IF NOT EXISTS license_deployment (
 id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
 instance_id VARCHAR(128) NOT NULL,
 customer_id VARCHAR(128) NOT NULL,
 environment VARCHAR(128) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
 current_digest VARCHAR(64) NOT NULL DEFAULT '',
 pending_digest VARCHAR(64) NOT NULL DEFAULT '',
 highest_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
 highest_observed_at BIGINT NOT NULL DEFAULT 0,
 clock_blocked BOOLEAN NOT NULL DEFAULT FALSE,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_license_instance (instance_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS license_artifact (
 digest VARCHAR(64) NOT NULL PRIMARY KEY,
 raw_jws MEDIUMTEXT NOT NULL,
 version BIGINT UNSIGNED NOT NULL,
 created_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS license_event (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 kind VARCHAR(64) NOT NULL,
 digest VARCHAR(64) NOT NULL DEFAULT '',
 version BIGINT UNSIGNED NOT NULL DEFAULT 0,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
 tenant_id VARCHAR(26) NOT NULL DEFAULT '',
 user_id VARCHAR(26) NOT NULL DEFAULT '',
 created_at DATETIME(3) NOT NULL,
 KEY ix_license_event_time (created_at,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS license_clock_recovery (
 id VARCHAR(128) NOT NULL PRIMARY KEY,
 license_digest VARCHAR(64) NOT NULL,
 anchor_at BIGINT NOT NULL,
 raw_jws MEDIUMTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO authz_resource (id,tenant_id,application_id,code,name,resource_type,attribute_schema,status,version,created_at,updated_at)
VALUES ('01J00000000000000000000600','01J00000000000000000000000','01J00000000000000000000001','commercial-license','商业授权管理','API',JSON_OBJECT(),'ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE name=VALUES(name),updated_at=UTC_TIMESTAMP(3);
INSERT INTO authz_permission (id,tenant_id,application_id,resource_id,code,action,name,description,risk_level,status,version,created_at,updated_at)
VALUES
 ('01J00000000000000000000601','01J00000000000000000000000','01J00000000000000000000001','01J00000000000000000000600','platform:license:read','read','查看商业授权','查看部署身份、商业授权及导入审计','LOW','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
 ('01J00000000000000000000602','01J00000000000000000000000','01J00000000000000000000001','01J00000000000000000000600','platform:license:manage','manage','管理商业授权','初始化部署、导入签名许可及消费时间恢复凭据','HIGH','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE name=VALUES(name),description=VALUES(description),updated_at=UTC_TIMESTAMP(3);
INSERT IGNORE INTO authz_role_permission (role_id,permission_id,effect,created_at)
SELECT r.id,p.id,'ALLOW',UTC_TIMESTAMP(3) FROM authz_role r JOIN authz_permission p ON p.tenant_id=r.tenant_id AND p.application_id=r.application_id
WHERE r.tenant_id='01J00000000000000000000000' AND r.application_id='01J00000000000000000000001' AND r.code='platform-super-admin' AND p.code IN ('platform:license:read','platform:license:manage');
INSERT INTO authz_policy_revision (tenant_id,application_id,revision,changed_at,change_reason)
VALUES ('01J00000000000000000000000','01J00000000000000000000001',18,UTC_TIMESTAMP(3),'新增商业授权管理')
ON DUPLICATE KEY UPDATE revision=GREATEST(revision,18),changed_at=UTC_TIMESTAMP(3),change_reason='新增商业授权管理';
