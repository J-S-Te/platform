# 生产备份、恢复与灾备演练

> 本文只展开备份介质与恢复门禁。首次安装、镜像导入、子系统接入和恢复后业务验收的完整顺序以
> [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md) 为唯一详细操作手册；服务器实测证据记录到
> [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md)。

本目录的备份介质包含数据库、运行配置、OAuth/OIDC 客户端密钥、字段加密材料、平台签名私钥和（启用 HTTPS 时）TLS 私钥。它等同于生产 Secret，禁止提交 Git、放入普通共享盘或复制到不受控终端。

## 安全策略

- `backups/`、批次目录固定为 `0700`，批次内普通文件固定为 `0600`；备份只能保存在 root 可读、具备磁盘加密和访问审计的介质上。
- 推荐通过 `--encryption-key-file` 生成 AES-256-CBC/PBKDF2 加密的恢复状态归档。密钥文件必须是普通文件、非空且权限为 `0400` 或 `0600`，并与备份介质分开保管。
- 未提供加密密钥时，数据库和恢复状态只依靠宿主机权限及底层加密存储保护；脚本会明确告警。不得把此模式的批次直接上传到对象存储或跨安全域传输。
- 脚本不会把 `.env`、runtime Secret、数据库密码、私钥内容写入日志。`VERSION_MANIFEST` 只记录镜像/版本指针。

## 创建完整灾备批次

完整备份与发布、接入、恢复共用 `runtime/.deploy.lock`。File Gateway 会在很短的写入冻结窗口内停止所有同一 Compose project 的 `file-gateway` 实例（包括已从 YAML 裁剪但仍运行的旧容器），完成数据库和对象目录同批次备份后恢复原实例。

```bash
cd /opt/unified-identity-platform
install -m 600 /secure/off-host/uip-backup.pass /root/.uip-backup.pass
sudo ./bin/backup-all.sh --encryption-key-file /root/.uip-backup.pass
```

批次位于 `backups/system/<UTC时间>/`，包含：

- 所有当时运行且受管的 MySQL 逻辑备份；
- File Gateway 的写入冻结数据库/对象文件批次；
- `.env`、`.release.env`、`runtime/`（含受管 TLS）、实际 `docker-compose.yml`、`subsystems.d/`、`subsystem-templates/`、`mysql-init/` 和恢复工具；
- `PLATFORM_KEYS_DIR` 指向的平台 JWT 签名密钥；
- `VERSION_MANIFEST`、非敏感恢复说明和全批次 `SHA256SUMS`。

创建命令会在提交正式批次前执行同一套完整性校验。任何数据库、密钥、HTTPS 材料或网关一致性批次缺失都会失败，不会把临时目录报告为成功备份。

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

恢复完成后先执行 `deploy.sh verify`、文件清单对账和上传/下载验收，再人工清理脚本保留的 rollback 目录。禁止为了排障删除数据库卷或对象目录。
