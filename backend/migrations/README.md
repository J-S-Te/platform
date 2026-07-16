# MySQL 迁移说明

- 迁移文件按 `000001_描述.sql` 连续编号；**已经在任何环境执行过的文件不得修改**，后续变更必须新增迁移文件。
- 从项目根目录运行 `make -C backend migrate`，或进入 `backend/` 后运行 `go run ./cmd/migrate`。命令默认读取 `backend/.env` 中的 MySQL 配置，也可用 `ENV_FILE` 指定其他环境文件。
- `platform_schema_migration` 记录版本、文件名、SHA-256 校验和与执行时间；同一版本的文件内容被修改时，迁移会拒绝继续执行。
- 迁移执行期间使用 MySQL `GET_LOCK` 避免并发改表。由于 MySQL DDL 可隐式提交，每条语句独立执行；SQL 需保持可安全重试。
- `000011_seed_platform_defaults.sql` 只创建默认租户、平台应用、开发环境、根组织、P0 权限、内建角色和 `platform.console` 配置命名空间。它**不会**创建带默认密码的管理员账号；首个管理员应在身份认证模块中使用 Argon2id 密码摘要显式创建。
