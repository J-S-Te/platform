# 生产备份、恢复与灾备演练

> 2026-10-22 源码核对；执行角色为获授权的生产运维及数据库管理员。本轮未执行备份、恢复或动态验收。

> 本文只展开备份介质与恢复门禁。首次安装、镜像导入、子系统接入和恢复后业务验收的完整顺序以
> [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md) 为唯一详细操作手册；服务器实测证据记录到
> [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md)。

本目录的备份介质包含数据库、运行配置、OAuth/OIDC 客户端密钥、字段加密材料、平台签名私钥和（启用 HTTPS 时）TLS 私钥。它等同于生产 Secret，禁止提交 Git、放入普通共享盘或复制到不受控终端。

## 安全策略

- `backups/`、批次目录固定为 `0700`，批次内普通文件固定为 `0600`；备份只能保存在 root 可读、具备磁盘加密和访问审计的介质上。
- 推荐通过 `--encryption-key-file` 生成 AES-256-CBC/PBKDF2 加密的恢复状态归档；此参数不加密数据库 SQL 或网关文件归档，仍须用加密介质保护整个批次。密钥文件必须是普通文件、非空且权限为 `0400` 或 `0600`，并与备份介质分开保管。
- 未提供加密密钥时，数据库和恢复状态只依靠宿主机权限及底层加密存储保护；脚本会明确告警。不得把此模式的批次直接上传到对象存储或跨安全域传输。
- 脚本不会把 `.env`、runtime Secret、数据库密码、私钥内容写入日志。`VERSION_MANIFEST` 只记录镜像/版本指针。

## 创建完整灾备批次

完整备份与发布、接入、恢复共用 `runtime/.deploy.lock`。File Gateway 会停止所有同一 Compose project 的 `file-gateway` 实例（包括已从 YAML 裁剪但仍运行的旧容器），完成数据库和对象目录同批次备份后恢复原实例；冻结时长取决于数据量和存储，不保证“很短”。备份后恢复进程失败仍返回失败，检查批次和实际运行状态。

```bash
cd /opt/unified-identity-platform
install -m 600 /secure/off-host/uip-backup.pass /root/.uip-backup.pass
sudo ./bin/backup-all.sh --encryption-key-file /root/.uip-backup.pass
```

批次位于 `backups/system/<UTC时间>-<进程号>/`，包含：

- 所有当时运行且受管的 MySQL 逻辑备份，以及 binlog 开启时每个备份旁的 `.binlog-position` 位点文件；
- File Gateway 的写入冻结数据库/对象文件批次；
- 已存在的 `portal-reports-data` 报告密文卷；按数据库快照后收集不可变对象，归档为 `portal-reports.tar.gz`，对应内容/描述符密钥保存在同批次 runtime 恢复状态中；
- `.env`、`.release.env`、`runtime/`（含受管 TLS）、实际 `docker-compose.yml`、`subsystems.d/`、`subsystem-templates/`、`mysql-init/` 和恢复工具；
- 规范批准目录/兼容根目录中的受支持运行批准及其独立完整性元数据（不存在时保留真实缺席，不虚构批准）；
- `PLATFORM_KEYS_DIR` 指向的平台 JWT 签名密钥；
- `VERSION_MANIFEST`、非敏感恢复说明和全批次 `SHA256SUMS`。

创建命令会在提交正式批次前执行同一套完整性校验。任何数据库、密钥、HTTPS 材料或网关一致性批次缺失都会失败，不会把临时目录报告为成功备份。

**当前备份范围限制**：中文入口安装的发行清单/签名/attestations快照位于 `runtime/release-integrity/`，随runtime恢复状态一起收集；它不是新信任根。宿主机独立verifier/trust、原始签名介质及各业务独立许可状态卷不由此批次自动归档/重装，需另行核验和保留。不能把一次SQL/配置解包写成已恢复全部发布信任与本地防回退状态。不用历史快照、随机新身份或删除卷来解除授权保护；恢复前核对平台当前身份/修订与受控登记模式。

## binlog 位点与重放

2026-10-23 起，逻辑备份在 MySQL 开启 binlog 时使用 `mysqldump --source-data=2`，在位点与事务快照一致的前提下把坐标写入独立 sidecar：

- 全库批次：`backups/system/<批次>/<service>.sql.gz.binlog-position`；
- Keycloak：`backups/keycloak/keycloak-<UTC时间>.sql.gz.binlog-position`；
- 内容为 `FORMAT=1`、`CAPTURED_AT`、`LOG_BIN`、`SOURCE_LOG_FILE`、`SOURCE_LOG_POS`，权限 0600；
- 全库 sidecar 纳入批次 `SHA256SUMS`；Keycloak sidecar 另有 `<sidecar>.sha256`；
- binlog 开启但取不到位点时该次备份失败；binlog 关闭时 sidecar 记录 `LOG_BIN=0` 并告警。

重放从 sidecar 的位点开始：

```bash
mysqlbinlog --start-position=<SOURCE_LOG_POS> <SOURCE_LOG_FILE> [后续 binlog...] | mysql -uroot
```

`--start-position` 只对第一个文件生效。位点只是起点，不包含 binlog 文件；宿主机整体丢失时仍只能全量恢复。binlog 保留期必须覆盖“恢复窗口 + 备份间隔”，否则最老备份的 binlog 可能已被清理。

## 升级和更新强制备份

中文服务器升级入口在导入候选及安装资产前要求原部署完整备份成功。新版镜像发布、前端发布、CRM/Portal 发布及受控启动入口也在修改运行配置和版本前执行完整备份；任何失败均停止。`--if-existing` 仅用于底层发布识别真正空安装：无既有数据库容器和保留卷才可无需历史备份，停止的库、残留卷、Docker 检查异常均拒绝。它不是运维跳过备份开关。

数据库迁移前仍保留单容器恢复点，并改为备份全部数据库（包括合同库承载的 Temporal、BI 库承载的 Metabase）。Shell 迁移恢复点为 SQL gzip 与旁路 SHA256；生产子系统更新器的恢复点为 `backups/<应用>-onboarding-<UTC时间>.sql.backup/`，其中 SQL 和 SHA256SUMS 以同一完整目录发布，空 SQL、命令失败、摘要失败均不得继续迁移。这种接入恢复点不能直接传给 `restore-mysql.sh`，恢复需专门审核及停写维护。

自动备份可能重复生成不同批次，部署前需评估空间与维护时长。文件网关会短暂停写，其他库使用在线逻辑快照；全系统跨库事务一致性需要额外维护停写，不由备份脚本保证。不得自动恢复数据库冒充镜像回滚，也不得借恢复回退授权版本。

## 只校验与空目录重建演练

校验不会连接 Docker 或修改在线数据，因此可在隔离恢复机上执行：

```bash
sudo ./bin/backup-all.sh \
  --verify-only \
  --backup /recovery-media/20260926T010203Z \
  --encryption-key-file /secure/separate/uip-backup.pass
```

空目录重建演练会再次校验整个批次，将受控配置、密钥、TLS 和恢复工具释放到指定空目录，并复制各数据库备份供隔离 MySQL 导入。它不接触当前部署、容器、卷或数据库：

```bash
sudo install -d -m 700 /srv/uip-rebuild-drill
sudo ./bin/backup-all.sh \
  --rebuild-drill \
  --backup /recovery-media/20260926T010203Z \
  --drill-root /srv/uip-rebuild-drill \
  --encryption-key-file /secure/separate/uip-backup.pass
```

报告归档存在时，重建演练同时解包到 `portal-reports/`，目录 `0700`、文件 `0600`。它只物化隔离文件，不替换线上报告卷。

真正的灾备验收还必须在隔离 Linux/AMD64 主机上安装与 `VERSION_MANIFEST` 匹配的不可变镜像，逐库导入 `database-backups/`，使用 File Gateway 隔离演练核对文件清单，最后验证 Keycloak Issuer/JWKS、一次登录、平台 API、Worker、Temporal workflow 和每个已启用子系统。仅解包成功不等于业务恢复完成。

## MySQL 恢复维护门禁

通用数据库恢复只接受 `backups/system/<批次>/<service>.sql.gz`。先校验：

```bash
sudo ./bin/restore-mysql.sh \
  --service project-mysql \
  --backup backups/system/<批次>/project-mysql.sql.gz \
  --verify-only
```

实际恢复在统一部署锁内扫描**全部运行中的 Docker 容器**：既按已知 API/Worker/迁移服务清单识别，也检查容器环境中的数据库主机引用，因此已经从当前 YAML 裁剪但仍运行的容器同样会阻止恢复。管理员必须先停止错误输出列出的所有写入者，再执行：

```bash
sudo ./bin/restore-mysql.sh \
  --service project-mysql \
  --backup backups/system/<批次>/project-mysql.sql.gz \
  --confirm RESTORE_MYSQL_SERVICE
```

`contract-mysql` 同时承载 Temporal 的 `temporal` 和 `temporal_visibility` 数据库，必须有独立维护审批并确认 Temporal 已停止：

```bash
sudo ./bin/restore-mysql.sh \
  --service contract-mysql \
  --backup backups/system/<批次>/contract-mysql.sql.gz \
  --confirm RESTORE_MYSQL_SERVICE \
  --confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE
```

`file-gateway-mysql` 禁止单库恢复，必须使用下一节的数据库+文件同批次恢复。

## File Gateway 恢复

先做不触碰生产数据的完整校验：

```bash
sudo ./bin/restore-file-gateway.sh \
  --backup backups/system/<批次>/file-gateway/<批次> \
  --verify-only
```

获批恢复会取得统一部署锁，停止当前 project 中所有 File Gateway 实例，拒绝其他仍引用 `file-gateway-mysql` 的隐藏写入者，原子替换文件目录并导入匹配数据库。恢复树会统一归属 `10001:10001`，目录模式 `0750`、文件模式 `0640`；数据库失败时文件目录回退、网关保持停止：

```bash
sudo ./bin/restore-file-gateway.sh \
  --backup backups/system/<批次>/file-gateway/<批次> \
  --confirm RESTORE_FILE_GATEWAY
```

数据库导入失败时旧文件目录可以回退，但数据库可能已部分导入，脚本不保证 SQL 回滚；网关保持停止，先人工评估数据库恢复。
恢复完成后先执行 `deploy.sh verify`、文件清单对账和上传/下载验收，再人工清理脚本保留的 rollback 目录。禁止为了排障删除数据库卷或对象目录。

## 隔离演练的容量边界

`drill-file-gateway-restore.sh` 使用 PLATFORM_IMAGE 内的 inventory 工具和临时 `mysql:8.4`，不是网关镜像直接启动；需提前导入镜像才能保证离线。
临时 MySQL 使用 1 GiB tmpfs，固定 `file_gateway` schema，并按排序最多检查 100000 条 READY 文件版本。
超过容量或扫描上限时不能记为全量验证；应记录实际 inspected 数并与 READY 总数核对，在经评审的隔离环境完成余量验证。
完整批次的 File Gateway 子批次名须从实际目录／MANIFEST 核对，不假定父批次带 PID 的名称与子批次完全相同。
`VERSION_MANIFEST` 来源于版本配置指针，不替代实际运行容器 digest 证据。只校验／空目录重建成功也不等于逐库导入和业务恢复完成。

## Portal 报告库与密文卷恢复

`MANIFEST` 的 `PORTAL_REPORTS_ARCHIVE=portal-reports.tar.gz` 表示该批次包含报告密文。先执行本页的 `--verify-only` 和空目录 `--rebuild-drill`；不要仅导入 Portal 数据库就启动服务。旧批次没有此声明，不能推断能恢复新报告。

正式恢复须进入已获批的停写维护窗口，持有统一部署锁并停止同一 Compose project 的全部 `portal-api`、`portal-report-worker` 及其他引用 `portal-mysql` 的写入者，包括已从 YAML 裁剪的容器。使用 `restore-mysql.sh --service portal-mysql` 的同批次 SQL，再将演练目录 `portal-reports/` 作为完整目录恢复到准确绑定的 `portal-reports-data` 卷；先保留旧卷副本，禁止在线覆盖、合并其他批次、删除原始备份或猜测卷名。以当前 Compose project 与 `com.docker.compose.volume=portal-reports-data` 标签核验唯一卷，恢复树应为 root 所有、目录 `0700`、普通文件 `0600`，不得含符号链接或特殊文件。

同时恢复同批次 Portal API/报告 worker 的 `PORTAL_REPORT_CONTENT_KEY_BASE64` 和 `PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64`，保持两文件一致、两种密钥互不相同；不要为恢复重新生成内容密钥。API/Worker 的身份凭据仍按当前控制面状态受控核对，不直接把历史 Secret 当现行有效凭据。先在隔离环境验证已签发报告能解密、其租户/请求/版本/摘要匹配、下载有真实水印与审计，再恢复线上消费者。任何失败保持停写并使用留存副本恢复，不宣称仅解包或数据库成功等同于报告恢复。

此处提供维护流程与隔离物化能力；本轮没有执行线上数据恢复。
