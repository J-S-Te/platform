CREATE DATABASE IF NOT EXISTS dashboard_metabase
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

GRANT ALL PRIVILEGES ON dashboard_metabase.* TO 'dashboard'@'%';
