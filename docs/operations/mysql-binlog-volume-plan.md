# MySQL binlog 减量与保留：只读核对清单与待批准变更方案

> 适用实例：`platform-mysql`、`keycloak-db`（生产 Compose，MySQL 8.4）。
> 编制：2026-10-23。依据为仓库源码与既有运维文档的只读核对。
> **本文不是执行授权。** 本轮未连接服务器、未执行 SQL、未修改参数、未清理 binlog。PURGE、参数变更、容器重建必须另行取得目标环境授权，并遵守 [BACKUP_RECOVERY.md](../../deploy/production/BACKUP_RECOVERY.md) 的备份与恢复门禁。

## 1. 结论摘要

1. 两个库都没有在 Compose 中固定 binlog 参数，沿用 `mysql:8.4` 镜像默认值：`ROW` + 全行前/后镜像 + 30 天过期 + 单文件 1 GiB + CRC32。
2. 生产 Compose 没有 MySQL 从库服务，仓库内也没有 Canal/Debezium 等 CDC 解析器。**行镜像能否收紧，取决于服务器上是否真的存在外部从库或 CDC，必须以 `SHOW REPLICAS` 和 Binlog Dump 连接为准。**
3. 现有数据库备份是 `mysqldump` 逻辑备份，`backup-all.sh` 与 `backup-keycloak-mysql.sh` 都**没有**使用 `--source-data`（旧名 `--master-data`），备份文件不携带 binlog 位点。因此当前不具备“从该备份起点重放 binlog”的完整 PITR 证据链。
4. 2026-09-07 磁盘满事故中两个库都出现过 `Disk is full writing './binlog.…'`，根分区当时 100%，处置后仍为 95%。binlog 是这次事故的组成部分，但不是唯一占用方。
5. 因此减量的正确顺序是：**先量化 → 确认备份可恢复 → 调保留期 → 开事务压缩 → 再评估行镜像**。不要一上来改 `binlog_row_image`。

## 2. 范围与边界

| 项 | 内容 |
| --- | --- |
| 本轮范围内 | `platform-mysql`（平台库）、`keycloak-db`（Keycloak 库） |
| 本轮范围外 | 合同库（含 Temporal）、CRM、门户、项目、结算、BI 库；本方案不把它们一起改 |
| 只读动作 | 查参数、查文件、查连接、查备份批次、采样体积 |
| 写动作（需单独授权） | `SET PERSIST/GLOBAL`、`FLUSH BINARY LOGS`、`PURGE BINARY LOGS`、修改 Compose、重建数据库容器 |
| 明确不执行 | 不连生产库、不写业务数据、不重启服务、不删除 binlog 文件、不 `RESET MASTER` |

## 3. 现状依据（源码可核对）

| 事实 | 证据 |
| --- | --- |
| 两库均为独立 `mysql:8.4` 容器 | [docker-compose.yml](../../deploy/production/docker-compose.yml) 的 `keycloak-db`、`platform-mysql` 服务 |
| `platform-mysql` 只覆盖字符集/排序规则 | 同文件 `command: ["--character-set-server=utf8mb4", "--collation-server=utf8mb4_0900_ai_ci"]` |
| `keycloak-db` 没有 `command` | 同上 |
| 全量备份不记录 binlog 位点 | [backup-all.sh](../../deploy/production/bin/backup-all.sh) 的 `mysqldump --single-transaction ... --all-databases` |
| Keycloak 单库备份同样不记录位点 | [backup-keycloak-mysql.sh](../../deploy/production/bin/backup-keycloak-mysql.sh) |
| 恢复入口与门禁 | [BACKUP_RECOVERY.md](../../deploy/production/BACKUP_RECOVERY.md)、[restore-mysql.sh](../../deploy/production/bin/restore-mysql.sh)、[restore-keycloak-mysql.sh](../../deploy/production/bin/restore-keycloak-mysql.sh) |
| Keycloak HA 文件默认不加载 | [keycloak-production-operations.md](../keycloak-production-operations.md) |
| 磁盘满事故记录 | [disk-full-login-outage-2026-09-07.md](./disk-full-login-outage-2026-09-07.md) |

> 行号会随源码变动，引用以文件当前内容为准。

## 4. 判定原则

1. **有无从库/CDC** 决定 `binlog_row_image` 能不能收紧；没有消费方才谈 `MINIMAL`。
2. **备份是否可恢复** 决定保留期能不能缩短；没有可验证备份就不 PURGE。
3. **是否对外声明 PITR** 决定要不要补 binlog 位点；只做逻辑备份恢复不等于 PITR。
4. **同 workload 对比**：改前改后取相同时段、相同业务量的 `SHOW BINARY LOGS` 总量增量，不跨高峰/低峰直接比较。
5. **一次只改一个参数**，观察窗口至少 24 小时，建议 7 天；每步留回滚值。

## 5. 清单 A：`platform-mysql` 只读核对

### A1. 定位实例与版本

```bash
cd /opt/unified-identity-platform
sudo docker compose --env-file .env --env-file .release.env -f docker-compose.yml \
  ps platform-mysql keycloak-db
sudo docker inspect -f '{{.Name}} {{.Config.Image}} {{.State.Status}}' \
  "$(sudo docker compose --env-file .env --env-file .release.env -f docker-compose.yml ps -q platform-mysql)"
```

记录实际镜像 digest，不只看 tag。

### A2. 参数基线

```bash
sudo docker exec platform-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "
SELECT @@version, @@version_comment,
       @@log_bin, @@binlog_format, @@binlog_row_image, @@binlog_row_metadata,
       @@binlog_row_value_options, @@binlog_transaction_compression,
       @@binlog_transaction_compression_level_zstd,
       @@binlog_expire_logs_seconds, @@binlog_expire_logs_auto_purge,
       @@max_binlog_size, @@sync_binlog, @@binlog_checksum,
       @@gtid_mode, @@server_id;"'
```

| 变量 | 8.4 默认（未覆盖时） | 本次记录值 |
| --- | --- | --- |
| `log_bin` | `ON` | |
| `binlog_format` | `ROW` | |
| `binlog_row_image` | `FULL` | |
| `binlog_row_metadata` | `MINIMAL` | |
| `binlog_row_value_options` | 空 | |
| `binlog_transaction_compression` | `OFF` | |
| `binlog_expire_logs_seconds` | `2592000`（30 天） | |
| `max_binlog_size` | `1073741824` | |
| `binlog_checksum` | `CRC32` | |

### A3. 规模与增长

```bash
# 当前文件列表与总字节
sudo docker exec platform-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "SHOW BINARY LOGS"'
sudo docker exec platform-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -Nse "SHOW BINARY LOGS"' \
  | awk '{s+=$NF} END {printf "binlog total: %.1f MiB\n", s/1048576}'

# 数据目录与宿主分区
sudo docker exec platform-mysql du -sh /var/lib/mysql
df -h /
```

连续 7 天，每天同一时段记录一次总字节和 `df -h /`。没有基线就不承诺“能省百分之多少”。

### A4. 依赖方与备份

```bash
# 8.4 使用 SHOW REPLICAS；旧语法 SHOW SLAVE HOSTS 仅作对照
sudo docker exec platform-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "SHOW REPLICAS; SHOW PROCESSLIST;"'

# 最近的完整备份批次与 Keycloak 单库备份
sudo ls -lt backups/system | head
sudo ls -lt backups/keycloak | head
```

核对：

- `SHOW REPLICAS` 是否为空；`SHOW PROCESSLIST` 是否有 `Binlog Dump` 线程。
- 最近一次 `backup-all.sh` 批次是否包含 `platform-mysql.sql.gz`，`SHA256SUMS` 是否通过 `--verify-only`。
- 业务/合规是否要求 30 天或更长的 binlog 保留；若要求，缩短保留期前先书面确认。
- 服务器上是否有仓库之外的 CDC、审计采集或同步程序连接该库。

### A5. 体积构成采样

```bash
# 取当前最大的一个历史文件；不要对正在写的文件做长时间解析
sudo docker exec platform-mysql sh -c 'ls -lS /var/lib/mysql/binlog.* | head -3'
sudo docker exec platform-mysql sh -c 'mysqlbinlog --statistics /var/lib/mysql/binlog.0000NN 2>/dev/null | tail -40'
```

看字节主要落在 `Write_rows` / `Update_rows` / `Delete_rows` 哪一类，以及是否有大字段表在反复更新。平台侧的会话、令牌、outbox 状态表可能是更新热点（以本步采样确认）；审计表以插入为主，行镜像对它帮助有限。

### A6. 记录表

| 日期 | 实例 | binlog 总量 | 当日增量 | `df -h /` 可用 | 备注 |
| --- | --- | --- | --- | --- | --- |
| | platform-mysql | | | | |

## 6. 清单 B：`keycloak-db` 只读核对

### B1. 定位实例与版本

```bash
cd /opt/unified-identity-platform
sudo docker compose --env-file .env --env-file .release.env -f docker-compose.yml \
  ps keycloak-db keycloak
sudo docker inspect -f '{{.Name}} {{.Config.Image}} {{.State.Status}}' \
  "$(sudo docker compose --env-file .env --env-file .release.env -f docker-compose.yml ps -q keycloak-db)"
```

### B2. 参数基线

```bash
sudo docker exec keycloak-db sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "
SELECT @@version, @@log_bin, @@binlog_format, @@binlog_row_image,
       @@binlog_row_metadata, @@binlog_row_value_options,
       @@binlog_transaction_compression, @@binlog_expire_logs_seconds,
       @@max_binlog_size, @@binlog_checksum, @@gtid_mode, @@server_id;"'
```

### B3. 规模与增长

命令同 A3，把 `platform-mysql` 换成 `keycloak-db`。Keycloak 的会话/凭据类表更新频繁、行宽较大，是行镜像收紧的主要潜在收益点，但仍以 B5 采样为准。

### B4. 依赖方与备份

```bash
sudo docker exec keycloak-db sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "SHOW REPLICAS; SHOW PROCESSLIST;"'
sudo ls -lt backups/keycloak | head
```

核对：

- 是否存在外部从库或 CDC；`compose.ha.keycloak.yaml` 默认不加载，但服务器上是否手工启用过要以实际容器/连接为准。
- 最近一次 `backup-keycloak-mysql.sh` 是否产出新的 `keycloak-*.sql.gz` 和 `.sha256`；独立备份锁并发时会跳过并返回零，必须确认确有新批次。
- 是否已做 `restore-keycloak-mysql.sh --verify-only`；没有验证过的备份不能作为 PURGE 的依据。

### B5. 体积构成采样

```bash
sudo docker exec keycloak-db sh -c 'ls -lS /var/lib/mysql/binlog.* | head -3'
sudo docker exec keycloak-db sh -c 'mysqlbinlog --statistics /var/lib/mysql/binlog.0000NN 2>/dev/null | tail -40'
```

### B6. 记录表

| 日期 | 实例 | binlog 总量 | 当日增量 | `df -h /` 可用 | 备注 |
| --- | --- | --- | --- | --- | --- |
| | keycloak-db | | | | |

## 7. 变更方案（分阶段，逐项批准）

### 7.1 P1：保留期与安全清理（两库，优先级最高）

**前提**

- 最近一次完整备份存在且 `--verify-only` 通过；Keycloak 另有独立单库备份。
- `SHOW REPLICAS` 为空、无 Binlog Dump 线程；或已确认所有从库消费位点早于待清理文件。
- 合规/审计确认的目标保留天数（示例用 7 天，实际值由业务定）。

**变更**

```sql
-- 目标值示例：604800 秒 = 7 天；先 SET PERSIST，后续随发布收敛到 Compose
SET PERSIST binlog_expire_logs_seconds = 604800;

-- 立即回收已有文件（先 FLUSH 让活跃文件切换；日期按实际保留截止时间替换）
FLUSH BINARY LOGS;
PURGE BINARY LOGS BEFORE '2026-10-16 00:00:00';
```

**注意**

- `PURGE` 不能删除当前正在写的文件，也不会更新备份；已清理的文件不可恢复。
- 不要用 `rm` 删 `/var/lib/mysql/binlog.*`，会破坏 `binlog.index`。
- 不要用 `RESET MASTER`（8.4 为 `RESET BINARY LOGS AND GTIDS`）代替清理。
- 修改 `binlog_expire_logs_seconds` 后，自动清理发生在轮转/启动等时机；要立刻回收就显式 `PURGE`。

**验证**

```sql
SHOW BINARY LOGS;
SELECT @@binlog_expire_logs_seconds;
```

外加 `df -h /`、`du -sh /var/lib/mysql`、错误日志无新磁盘错误。

**回滚**

```sql
SET PERSIST binlog_expire_logs_seconds = 2592000;
```

已 PURGE 的文件无法回滚；执行前必须确认备份可用。

**中止条件**：备份校验失败、存在未确认的从库/CDC、合规保留要求未确认、`df` 在清理后仍持续下降。

### 7.2 P2：事务压缩（两库，语义不变）

**前提**：8.4 且解析工具支持 zstd 压缩事务（本工程仓库内无 CDC；外部工具需逐一确认）。

**变更**

```sql
SET PERSIST binlog_transaction_compression = ON;
SET PERSIST binlog_transaction_compression_level_zstd = 3;
```

**验证**

- 同 workload 窗口的 binlog 增量对比；小事务收益有限，不要按比例预承诺。
- `mysqlbinlog` 能正常解析新文件（8.0.20+ 客户端自动解压）。
- `docker stats --no-stream platform-mysql keycloak-db` 的 CPU 在限额内（当前 Compose 为 `CONTAINER_CPU_LIMIT`，默认 2.0）。
- 错误日志无压缩相关告警。

**回滚**

```sql
SET PERSIST binlog_transaction_compression = OFF;
```

**中止条件**：CPU 持续贴近限额、解析/备份工具报错、业务延迟上升。

### 7.3 P3：`binlog_row_image=NOBLOB`（低风险）

**前提**：P1/P2 已稳定观察；确认无消费方依赖“未变化的 BLOB/TEXT 也出现在事件里”。

**变更**

```sql
SET PERSIST binlog_row_image = 'NOBLOB';
```

**验证**：binlog 增量、错误日志、一次隔离恢复演练、平台 readyz 与测试账号登录、Keycloak discovery/JWKS 与测试账号登录。

**回滚**

```sql
SET PERSIST binlog_row_image = 'FULL';
```

### 7.4 P4：`binlog_row_image=MINIMAL`（可选，收益最大）

**前提（缺一不可）**

- 无外部从库、无 CDC、无依赖完整前镜像的审计/同步程序。
- 热点表有主键；无主键表在 MINIMAL 下仍会记录整行，收益有限。
- 已完成至少一次“备份 + binlog 重放”的隔离演练。
- 业务方接受“事件里看不到未变更列”的语义变化。

**变更**

```sql
SET PERSIST binlog_row_image = 'MINIMAL';
```

**验证**

- 隔离实例导入最近备份，重放一段 binlog，核对关键表行数与抽样校验和。
- 平台登录、会话续期、审计写入；Keycloak 登录、刷新、登出。
- 错误日志无 1032/1062 类复制/重放错误。

**回滚**

```sql
SET PERSIST binlog_row_image = 'FULL';
```

**中止条件**：任何消费方解析失败、隔离重放不一致、业务登录异常。

### 7.5 P5：应用侧减写（无参数风险，优先级仅次于 P1）

- 只更新真正变化的列，避免整行回写；尤其会话、令牌、outbox 状态类表。
- outbox 重试加退避，减少“失败一次就整行更新一次”的写放大。
- 大 payload 不放在高频更新的热表；必要时归档冷数据。
- Keycloak 侧不修改内部表结构；通过会话/离线会话策略减少写入。

### 7.6 明确不做

| 做法 | 原因 |
| --- | --- |
| 全局切 `binlog_format=STATEMENT/MIXED` | 非确定性语句不安全，且直接废掉行级解析；8.4 已将 `binlog_format` 标记为弃用 |
| 源端 `binlog_do_db` / `binlog_ignore_db` | 只按默认库过滤，带库名限定的 SQL 会漏；被过滤库失去 PITR |
| 应用里 `SET sql_log_bin=0` | 变更不进复制/恢复链，主从或灾备切换即分叉 |
| 关闭 binlog | 放弃 PITR 与复制，需重启，不能作为省盘手段 |
| `binlog_checksum=NONE` | 每事件只省 4 字节，丢掉损坏检测 |
| `rm` binlog 文件 / `RESET MASTER` | 破坏索引与 GTID 历史，不可逆 |

## 8. 持久化方式与回滚矩阵

`SET GLOBAL` 只对当前进程有效；容器重建或重启后回到镜像默认。`SET PERSIST` 写入数据目录下的 `mysqld-auto.cnf`，可跨重启，但形成 Compose 之外的配置漂移。

推荐路径：

1. 变更窗口内用 `SET PERSIST` 生效并登记。
2. 随后把最终参数写入 [docker-compose.yml](../../deploy/production/docker-compose.yml) 的 `command:`（`platform-mysql` 已有 command 可追加；`keycloak-db` 需新增），走发布评审。
3. 容器按新参数重建并核对 `SELECT @@...` 后，执行 `RESET PERSIST <变量>` 清除数据目录内的覆盖，避免两处配置打架。

| 变量 | 8.4 默认 | 目标（示例） | 回滚 |
| --- | --- | --- | --- |
| `binlog_expire_logs_seconds` | `2592000` | `604800` | 改回 `2592000`；已清理文件不可恢复 |
| `binlog_transaction_compression` | `OFF` | `ON` | `OFF` |
| `binlog_row_image` | `FULL` | `NOBLOB` → `MINIMAL` | `FULL` |
| `binlog_row_value_options` | 空 | 暂不动 | 置空 |

修改 Compose 会重建数据库容器（数据卷保留），必须有维护窗口和可用备份；生产入口遵循 `deploy.sh`，不要用裸 `docker compose up` 绕过门禁。

## 9. 验证与验收

| 层面 | 检查 |
| --- | --- |
| 参数 | `SELECT @@binlog_row_image, @@binlog_transaction_compression, @@binlog_expire_logs_seconds` 与目标一致 |
| 容量 | 7 天同 workload 的 binlog 增量、`df -h /`、`du -sh /var/lib/mysql` |
| 复制/重放 | `SHOW REPLICAS`、错误日志无 1032/1062；隔离实例重放一致 |
| 备份 | `backup-all.sh --verify-only` 通过；Keycloak `restore-keycloak-mysql.sh --verify-only` 通过 |
| 平台 | `deploy.sh verify`、readyz、授权测试账号登录、审计可查 |
| Keycloak | discovery/JWKS、测试账号登录/刷新/登出、管理端只读页面正常 |
| 性能 | `docker stats` CPU/内存未贴近限额；业务延迟无抬头 |

## 10. 风险与未决

1. **PITR 缺口**：备份不记录 binlog 位点。若要对“恢复到某一时刻”负责，已在 2026-10-23 实施（见 [MySQL 备份补记 binlog 位点](./mysql-backup-binlog-position-plan.md)），但仍需在目标环境完成备份与重放验证；不能只靠本方案的参数调整。
2. **未来从库/CDC**：当前无 Compose 从库，但 `compose.ha.keycloak.yaml` 存在；一旦启用 HA 或接入 CDC，`MINIMAL` 需要重新评估。
3. **合规保留**：30 天是镜像默认，不一定是业务要求；缩短前要有人对保留期负责。
4. **压缩成本**：zstd 吃 CPU，数据库容器本身有 2.0 CPU 限额，先看基线。
5. **配置漂移**：`SET PERSIST` 不会出现在 Compose 里，必须登记并最终收敛。
6. **磁盘仍可能满**：binlog 只是占用之一，备份批次、日志驱动、镜像层都要一起治理。

## 11. 执行记录模板

| 字段 | 内容 |
| --- | --- |
| 时间（Asia/Shanghai） | |
| 实例 | platform-mysql / keycloak-db |
| 授权单/批准人 | |
| 变更前参数 | |
| 执行命令 | |
| 执行结果 | |
| binlog 总量/增量 | |
| 磁盘可用 | |
| CPU/内存 | |
| 备份校验 | |
| 业务验证 | |
| 回滚点 | |
| 结论 | 保留 / 回滚 |

记录存放：服务器受控运维目录；仓库内只保留脱敏摘要，不写密码、Token、连接串或业务数据。

## 12. 参考

- [生产备份、恢复与灾备演练](../../deploy/production/BACKUP_RECOVERY.md)
- [Keycloak 生产运维说明](../keycloak-production-operations.md)
- [生产编排 docker-compose.yml](../../deploy/production/docker-compose.yml)
- [2026-09-07 磁盘满导致登录异常](./disk-full-login-outage-2026-09-07.md)
- [backup-all.sh](../../deploy/production/bin/backup-all.sh)、[backup-keycloak-mysql.sh](../../deploy/production/bin/backup-keycloak-mysql.sh)
