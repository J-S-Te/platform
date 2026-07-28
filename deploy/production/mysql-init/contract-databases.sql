CREATE DATABASE IF NOT EXISTS temporal CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS temporal_visibility CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON temporal.* TO 'contract'@'%';
GRANT ALL PRIVILEGES ON temporal_visibility.* TO 'contract'@'%';
FLUSH PRIVILEGES;
