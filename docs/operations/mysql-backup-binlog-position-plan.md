# MySQL 备份补记 binlog 位点：改动方案（已实施）

> 状态：2026-10-23 已按本方案实施并通过本地测试；未部署、未连接生产库、未执行真实备份或恢复。
> 实施范围：`bin/backup-all.sh`、`bin/backup-keycloak-mysql.sh`、`bin/test-backup-recovery-safety.sh`、`tests/test-pre-update-backup.py`、`tests/test-runtime-approval-publisher.py`。
> 关联：[binlog 减量与保留方案](./mysql-binlog-volume-plan.md)、[生产备份与恢复](../../deploy/production/BACKUP_RECOVERY.md)。

## 1. 问题

现有逻辑备份只保证“能恢复到备份开始那一刻”，不保证“能继续重放到之后的某一刻”：

- `backup-all.sh` 的 `mysqldump` 使用 `--single-transaction`，但没有 `--source-data`，备份文件里没有 binlog 文件名和位点。
- `backup-keycloak-mysql.sh` 同样没有记录位点。
- 因此“备份 + binlog 重放”的 PITR 链缺少起点；恢复脚本只能整库导入，无法声明可以恢复到某个时间点。

这属于证据链缺口，不是脚本报错；日常备份看起来一直是成功的。

## 2. 目标与非目标

### 目标

1. `platform-mysql` 与 `keycloak-db` 的每次逻辑备份，都产出与快照一致的 binlog 位点。
2. 位点作为独立 sidecar 文件落盘，纳入现有校验，不破坏旧批次。
3. binlog 开启但取不到位点时，备份失败关闭（fail closed），不产出“看似可 PITR”的备份。
4. 文档给出重放命令和与保留期的耦合规则。

### 非目标

- 不做物理备份、不替换 `mysqldump`。
- 不把 binlog 文件本身复制到备份批次；宿主机丢失时仍只有全量恢复能力。
- 不在本期改 `deploy-service.sh`、`backup-file-gateway.sh`、`deploy-customer-opportunity.sh`、`ops-linux.sh` 的转储。
- 不改变 `restore-mysql.sh` 的破坏性恢复门禁。

## 3. 现状证据

| 事实 | 位置（2026-10-23 核对） |
| --- | --- |
| 全库转储无位点 | `platform/deploy/production/bin/backup-all.sh` 约 401 行 |
| 转储后做 gzip、非空、SHA 校验 | 同文件约 406-410、510 行 |
| 批次 `MANIFEST` 只记数据库数量 | 同文件约 486-498 行 |
| `--verify-only` 只按 `*.sql.gz` 计数 | 同文件约 192-214 行 |
| Keycloak 单库转储无位点 | `backup-keycloak-mysql.sh` 约 64-67 行 |
| Keycloak 校验和只覆盖 `.sql.gz` | 同文件约 73-77 行；恢复端读取 `.sha256` 见 `restore-keycloak-mysql.sh` 约 97-108 行 |
| 恢复端直接 `gzip -dc | mysql` | `restore-mysql.sh` 约 147 行 |
| 发布前另有转储，同样无位点 | `deploy-service.sh` 约 736-739 行 |
| 网关停写快照，整批恢复，不需要位点 | `backup-file-gateway.sh` 约 138 行 |
| 资产清单是显式枚举 | `build-offline-packages.sh` 约 804 行 |
| 运行批准发布只复制固定文件 | `publish-runtime-approvals.py` 约 535-538 行 |

## 4. 方案设计

### 4.1 为什么用 `--source-data=2`

`mysqldump --source-data=2` 会把位点写成 SQL 注释：

```sql
-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000069', SOURCE_LOG_POS=436217060;
```

- 用 `2` 而不是 `1`：`1` 会写成可执行的 `CHANGE REPLICATION SOURCE TO`，整库导入时可能意外改写复制配置；注释形式对独立实例导入无副作用。
- 不用“备份前后单独查 `SHOW BINARY LOG STATUS`”：那和 `--single-transaction` 的快照不在同一一致性点上，重放会多算或少算事务。
- 不用物理备份：当前栈没有 XtraBackup，引入成本高于本改动。

### 4.2 一致性窗口与权限

- `--source-data` 必须和 `--single-transaction` 一起用。后者已经在两个脚本里；缺少它时 `--source-data` 会退化成整库锁表，不能接受。
- 开启 `--source-data` 后，`mysqldump` 会在开始时短暂持有全局读锁（`FLUSH TABLES WITH READ LOCK`），用于把位点和事务快照对齐，然后立即释放。这是毫秒级写入停顿，但高并发时可能排队，需要观察。
- 需要 `RELOAD` 权限。当前脚本用 `root`，已具备；若未来改为最小权限账号，必须显式授予。

### 4.3 位点提取

在 `gzip -t` 和非空检查之后，从压缩流头部提取第一行匹配，避免解压整个文件：

```bash
extract_binlog_position() {
  local dump="$1" line source_file source_pos
  line="$(gzip -dc -- "$dump" 2>/dev/null | head -n 40 | grep -m1 '^-- CHANGE REPLICATION SOURCE TO ' || true)"
  [[ -n "$line" ]] || return 1
  source_file="$(printf '%s\n' "$line" | grep -o "SOURCE_LOG_FILE='[^']*'" | cut -d"'" -f2)"
  source_pos="$(printf '%s\n' "$line" | grep -o 'SOURCE_LOG_POS=[0-9][0-9]*' | cut -d= -f2)"
  [[ -n "$source_file" && -n "$source_pos" ]] || return 1
  printf '%s\t%s\n' "$source_file" "$source_pos"
}
```

管道末尾的 `|| true` 是必要的：`head` 提前关闭管道会让 `gzip` 收到 SIGPIPE，在 `set -Eeuo pipefail` 下会把正常的“只读前 40 行”判成失败。

2026-10-23 已用四个合成样本验证：正常位点解析成功；缺位点、文件号无引号、位置非数字均按预期失败并返回 1。`mysqldump --source-data=2` 固定给文件号加引号，因此这里按带引号格式解析，格式不符就失败关闭。

### 4.4 sidecar 格式

文件名与备份文件同目录、同前缀，权限 0600：

- 全库批次：`backups/system/<批次>/platform-mysql.sql.gz.binlog-position`
- Keycloak：`backups/keycloak/keycloak-<UTC时间>.sql.gz.binlog-position`

内容：

```ini
FORMAT=1
CAPTURED_AT=2026-10-23T03:44:40Z
LOG_BIN=1
SOURCE_LOG_FILE=binlog.000069
SOURCE_LOG_POS=436217060
```

binlog 关闭的实例写 `LOG_BIN=0`，不写位点，并输出一次告警；这样不会因为单个库关闭 binlog 就让整批灾备失败。

### 4.5 失败策略

| 情况 | 行为 |
| --- | --- |
| `SELECT @@log_bin` 返回 1，位点提取成功 | 正常写 sidecar |
| 返回 1，位点缺失或格式非法 | 备份失败，删除临时文件，不产出批次 |
| 返回 0 | 转储不带 `--source-data`，sidecar 写 `LOG_BIN=0`，告警 |
| 查询本身失败 | 备份失败（无法判断是否具备 PITR 能力） |

### 4.6 `backup-all.sh` 集成

1. 在容器运行检查之后，查询 `SELECT @@log_bin`。
2. 用数组拼转储参数；`log_bin=1` 时加 `--source-data=2`。
3. 通过 `sh -ec '... mysqldump "$@" ...' _ "${dump_args[@]}"` 传参，避免把可变参数拼进固定字符串。
4. 在 `gzip -t`、非空检查之后、`mv` 之前提取位点，写 `$work/<service>.binlog-position`。
5. `MANIFEST` 增加 `BINLOG_POSITIONS=required`。
6. `verify_disaster_batch` 只在 `BINLOG_POSITIONS=required` 时检查每个 `*.sql.gz` 都有非空 sidecar；旧批次没有该键，继续通过。
7. `SHA256SUMS` 的生成逻辑不用改，sidecar 会自动被现有 `find ... sha256sum` 覆盖。

### 4.7 `backup-keycloak-mysql.sh` 集成

1. 在现有 `mysqladmin ping` 预检中附加 `SELECT @@log_bin`，或单独执行一次。
2. 转储参数数组按 `log_bin` 追加 `--source-data=2`。
3. 提取位点，写 `$final_file.binlog-position`。
4. sidecar 单独生成 `.binlog-position.sha256`；`.sql.gz.sha256` 保持只覆盖 SQL，避免 `restore-keycloak-mysql.sh` 在无 `sha256sum` 的平台上的单行解析被第二行干扰。
5. `cleanup` 同时删除未完成的 sidecar 和它的 `.sha256`。
6. 保留期清理的 `find` 增加 `keycloak-*.sql.gz.binlog-position` 与对应 `.sha256`，避免只删 SQL 留下孤儿文件。
7. 指标沿用“上次成功时间”；不新增按位点变化的 gauge，避免标签抖动。

### 4.8 恢复与 roll-forward

SQL 注释在导入时会被 `mysql` 客户端忽略，现有恢复脚本不需要改导入逻辑。真正重放时：

```bash
mysqlbinlog --start-position=436217060 binlog.000069 binlog.000070 binlog.000071 | mysql -uroot
```

- `--start-position` 只对第一个文件生效，后续文件从文件头开始。
- 必须使用与备份同源的 binlog 文件；文件已被 `PURGE` 时无法重放。
- 建议加 `--stop-datetime` 或 `--stop-position`，避免把故障后的写入也重放进去。

### 4.9 与保留期的耦合

设备份间隔为 `I`、期望可恢复到最近 `W` 内的任意时刻，则：

```text
binlog 保留时长 T >= W + I + 余量
```

例：每天备份一次（`I=24h`），希望恢复最近 7 天（`W=168h`），则 `T >= 192h`，即至少 8 天。只把保留期设成 7 天会让最老的那份备份缺少可用 binlog。

这也是 [binlog 减量与保留方案](./mysql-binlog-volume-plan.md) 里 P1 的前置条件：缩短保留期之前，先确认备份频率和恢复窗口。

### 4.10 为什么内联实现而不是公共 helper

新增 `bin/mysql-binlog-position.sh` 看似更整洁，但有两个固定清单要同步：

- `build-offline-packages.sh` 的 `asset_files` 是显式枚举，新增文件要改打包清单。
- `publish-runtime-approvals.py` 只把 `bin/backup-all.sh`、`bin/lifecycle.sh`、`bin/provisioner-config-refresh.sh` 复制到维护根目录；`backup-all.sh` 若 `source` 一个未复制的 helper，会在发布运行批准时失败。

因此本期把约 10 行函数内联到两个脚本，复制成本低于清单漂移风险。等第三个转储点也要用时再抽公共文件。

## 5. 逐文件改动清单

| 文件 | 改动 | 风险 |
| --- | --- | --- |
| `bin/backup-all.sh` | 查 `log_bin`、动态参数、提取位点、sidecar、MANIFEST 键、verify 守卫 | 中；涉及灾备主路径 |
| `bin/backup-keycloak-mysql.sh` | 同上（单库版）、sidecar 校验、保留期清理 | 中 |
| `bin/restore-mysql.sh` | 可选：`--print-binlog-position` 读取并打印 sidecar | 低 |
| `bin/test-backup-recovery-safety.sh` | 静态断言、`bash -n` 覆盖 Keycloak 脚本、sidecar 断言 | 低 |
| `tests/test-pre-update-backup.py` | docker 替身需响应 `SELECT @@log_bin` 并输出带注释的转储；补成功路径 | 低 |
| `BACKUP_RECOVERY.md` | 新增“位点 sidecar 与 roll-forward”一节 | 低 |
| `docs/keycloak-production-operations.md` | 说明 sidecar、校验和与保留期 | 低 |
| `docs/operations/mysql-binlog-volume-plan.md` | 把 PITR 缺口链接到本方案 | 低 |

不改：`docker-compose.yml`、`restore-keycloak-mysql.sh` 的导入逻辑、`backup-file-gateway.sh`。

## 6. 测试计划

### 6.1 静态

```bash
bash -n platform/deploy/production/bin/backup-all.sh
bash -n platform/deploy/production/bin/backup-keycloak-mysql.sh
bash platform/deploy/production/bin/test-backup-recovery-safety.sh
```

断言至少包括：

- 两个脚本都出现 `--source-data=2` 和 `--single-transaction`。
- 两个脚本都出现 `SELECT @@log_bin` 和 `binlog-position`。
- `backup-all.sh` 的 `MANIFEST` 含 `BINLOG_POSITIONS=required`。
- Keycloak 的保留期 `find` 覆盖 sidecar。

### 6.2 解析单元

用合成 gzip 覆盖：正常、缺失、无引号文件号、非数字位置。已在本方案阶段验证；实现后把同一组样例固化进测试脚本。

### 6.3 Keycloak 端到端（假 docker）

在临时目录放一个 `docker` 替身：

- `compose config -q` 返回 0；
- `compose exec ... mysqladmin` 返回 0；
- `compose exec ... SELECT @@log_bin` 输出 `1`；
- `compose exec ... mysqldump` 输出带 `-- CHANGE REPLICATION SOURCE TO` 注释的 SQL。

断言产出 `.sql.gz`、`.binlog-position`、两个 `.sha256`；sidecar 解析出预期文件和位点；模拟保留期清理后三类文件同时消失。

### 6.4 隔离 MySQL 导入与重放

在一次性 `mysql:8.4` 容器中：

1. 开启 binlog（镜像默认已开），建库建表并插入基线行。
2. 用带 `--source-data=2` 的 `mysqldump` 备份。
3. 备份后继续插入一行，记录当前 binlog 位点。
4. 在另一个空实例导入备份，按 sidecar 位点重放 binlog。
5. 断言基线行和增量行都存在，且没有重复。

这一步必须在隔离环境执行，不连接业务库；它是“真的能 PITR”的唯一证据。

### 6.5 回归

- `platform/deploy/production/tests/test-pre-update-backup.py`
- `platform/deploy/production/tests/test-runtime-approval-publisher.py`
- `platform/deploy/production/tests/test-oneclick-upgrade-preflight.py`
- `platform/deploy/production/bin/test-operations-tool.sh`
- `platform/deploy/production/bin/test-start-enabled.sh`

Linux 上还要跑归档级动态用例；macOS 会跳过 GNU tar/find/flock 相关分支，不能把跳过当通过。

### 6.6 验收标准

1. 新批次每个数据库都有 sidecar，`--verify-only` 通过，`SHA256SUMS` 校验通过。
2. 备份后立即比对 `SHOW BINARY LOG STATUS`：sidecar 位点应落在“备份前位点”和“备份后位点”之间，且文件号不回退。
3. 隔离环境导入加重放成功。
4. 备份失败率不上升；`FLUSH TABLES WITH READ LOCK` 造成的写入停顿有实测记录。
5. 文档更新完成，旧批次仍可校验。

## 7. 发布与回滚

- 改动只涉及 `bin/` 下的脚本和文档，不需要数据库迁移。
- 离线资产包需要重新构建：`bin/build-offline-packages.sh` 会重新计算资产摘要；服务器用 `bin/install-assets.sh` 受控安装。
- 先在备份窗口手工执行一次 `backup-all.sh`，核对 sidecar 和 `--verify-only`，再恢复定时任务。
- 回滚：恢复上一版两个脚本即可；已生成的 sidecar 对旧脚本无影响，旧批次没有 sidecar 也继续通过校验。
- 不通过时不要删除已有批次，也不要为了“让校验通过”修改 `SHA256SUMS`。

## 8. 风险

| 风险 | 影响 | 处置 |
| --- | --- | --- |
| `FLUSH TABLES WITH READ LOCK` 短暂停顿 | 备份开始瞬间写入排队 | 低峰执行；观测应用延迟和锁等待；必要时降低备份频率 |
| 最小权限账号缺 `RELOAD` | 转储直接失败 | 当前 root 不受影响；改账号时补权限并测试 |
| GTID 开启后的 `--set-gtid-purged` | 导入非空实例可能失败 | 当前 `gtid_mode=OFF`；开启前单独评审，或导入时用 `--set-gtid-purged=OFF` |
| 保留期短于恢复窗口 | 有位点但没有 binlog 可重放 | 按 4.9 的公式设置；把两者写进同一变更单 |
| 宿主机整体丢失 | binlog 随主机消失，只能全量恢复 | 明确 RPO 等于备份间隔；异地 binlog 归档另立任务 |
| 备份与 binlog 同盘 | 磁盘满时两者一起受损 | 监控、扩容、异地副本；不要把 sidecar 当成异地备份 |
| 误把 sidecar 当备份 | 恢复失败 | 文档和文件名都明确它是元数据；恢复入口只接受 `.sql.gz` |

## 9. 未决问题

1. 位点缺失时是否默认 fail closed：本方案建议“binlog 开启即失败”，binlog 关闭则告警继续。需要运维确认。
2. `BINLOG_POSITIONS=required` 是否进入 `verify_disaster_batch` 的强制项：建议新批次强制、旧批次兼容。
3. 是否本期一并改 `deploy-service.sh`：建议二期，因为它会让每次发布都多一次短暂全局读锁。
4. 是否做 binlog 异地归档：建议单独立项，涉及带宽、加密和清理策略。
5. GTID 开启后的统一策略：保持 AUTO 还是固定 `OFF`，需要和未来复制方案一起定。

## 10. 实施与验证记录（2026-10-23）

已修改：

- `bin/backup-all.sh`：查询 `@@log_bin`，按状态追加 `--source-data=2`，写 `<service>.sql.gz.binlog-position`；`MANIFEST` 增加 `BINLOG_POSITIONS=required`，`verify_disaster_batch` 对新批次校验 sidecar。
- `bin/backup-keycloak-mysql.sh`：同样记录位点；sidecar 独立 SHA-256；保留期清理覆盖 sidecar 与它的校验和。
- `bin/test-backup-recovery-safety.sh`：静态断言、从脚本正文提取解析/写入函数做失败关闭测试、假 docker 端到端。
- `tests/test-pre-update-backup.py`：更新 docker 替身以响应 `SELECT @@log_bin` 并输出带位点注释的转储；为 macOS 补 `flock`/Bash 4 描述符适配，`/proc` 专属分支在非 Linux 跳过。
- `tests/test-runtime-approval-publisher.py`：备份替身增加位点查询与注释输出。

本地结果：

- `bash bin/test-backup-recovery-safety.sh`：通过。
- `python3 tests/test-pre-update-backup.py`：6 项通过。
- `python3 tests/test-runtime-approval-publisher.py -k test_real_disaster_backup_captures_and_verifies_runtime_approvals`：通过。
- `test-lifecycle.sh`、`test-start-enabled.sh`：通过。
- `test-oneclick-upgrade-preflight.py`：2 项通过、16 项按环境跳过。

未执行：真实 MySQL 导入与 binlog 重放、对生产库的备份、Linux 专属归档分支、部署。`tests/test-operations-tool.sh` 在本机因 sudo 权限限制未完成，与本改动无关。

## 11. 参考

- [MySQL 8.4 mysqldump 文档](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)
- [生产备份与恢复](../../deploy/production/BACKUP_RECOVERY.md)
- [Keycloak 生产运维](../keycloak-production-operations.md)
- [binlog 减量与保留方案](./mysql-binlog-volume-plan.md)
- [backup-all.sh](../../deploy/production/bin/backup-all.sh)、[backup-keycloak-mysql.sh](../../deploy/production/bin/backup-keycloak-mysql.sh)
