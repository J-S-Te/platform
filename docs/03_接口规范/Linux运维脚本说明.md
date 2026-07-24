# Linux 运维自动化脚本说明

> **适用范围**：`scripts/ops-linux.sh` 面向“原生二进制 + systemd + Nginx + MySQL”的 Linux 单机或单应用服务器部署。它用于自动化重复性运维工作，**不是** Docker Compose 管理脚本。
>
> **安全边界**：该脚本可减少日常人工操作，但不能替代密钥保管、漏洞修复、异常研判、数据库恢复演练和上线审批责任。生产变更仍应由具备服务器与数据库权限的责任人复核。
>
> **大小写不可变更**：Linux 区分大小写。`basic-platform-api.service`、`basic-platform-worker.service`、`ENV_FILE`、`FILE_STORAGE_ROOT`、`/healthz`、`/readyz` 以及本文所有路径和命令必须按原样使用。

---

## 1. 能力与边界

脚本文件：`scripts/ops-linux.sh`。

它覆盖以下重复性工作：

| 能力 | 命令 | 结果 |
| --- | --- | --- |
| 部署巡检 | `doctor` | 检查环境文件、发布目录、当前版本、systemd 单元、服务、健康检查、Nginx 和磁盘 |
| 状态和健康检查 | `status`、`health` | 查看当前版本、API/Worker 状态，访问 `/healthz` 和 `/readyz` |
| 日志查看 | `logs` | 统一读取 API 和 Worker 的 `journalctl` 日志 |
| 服务控制 | `start`、`stop`、`restart` | 控制两个 systemd 服务；启动和重启后等待 API 就绪 |
| 数据库迁移 | `migrate` | 先备份 MySQL，再使用当前发布版本的迁移程序执行迁移 |
| 备份 | `backup` | 备份 MySQL 和 `FILE_STORAGE_ROOT`，并生成 SHA-256 校验文件 |
| 受控发布 | `deploy` | 先备份数据库，再调用 `scripts/bootstrap-linux.sh --deploy` 执行构建、迁移、原子切换与服务重启 |
| 应用版本回退 | `rollback` | 切换 `/opt/basic-platform/current` 软链接并重启服务；不会回退数据库 |
| Nginx 平滑重载 | `reload-nginx` | 先执行 `nginx -t`，再执行 `systemctl reload nginx` |
| 备份清理 | `prune-backups` | 清理本脚本创建且超过保留天数的 MySQL、上传目录备份与校验文件 |

脚本**不会**做以下事情：

- 仅在进程内读取备份所需的数据库和文件目录配置；不会输出、写入日志或上传密码、JWT 私钥、应用加密密钥、OAuth 客户端密钥等敏感值；
- 不会把 `/etc/basic-platform/basic-platform.env` 或私钥复制进普通备份目录；
- 不会替生产环境申请或续期 TLS 证书；
- 不会在发布失败后自动回退应用，因为已执行的数据库迁移可能使旧版本不兼容；
- 不会自动执行数据库 `down migration`；
- 不会取代异机备份、灾难恢复演练和人工安全审批。

## 2. 前置条件

### 2.1 部署方式与路径

脚本默认使用以下生产约定；可用环境变量覆盖，见第 3 节。

| 项目 | 默认值 | 说明 |
| --- | --- | --- |
| 发布根目录 | `/opt/basic-platform` | 含 `releases/` 与 `current` 软链接 |
| 环境文件 | `/etc/basic-platform/basic-platform.env` | API、Worker、迁移程序共同使用 |
| API 服务 | `basic-platform-api.service` | systemd 单元名 |
| Worker 服务 | `basic-platform-worker.service` | systemd 单元名 |
| 运行用户 | `basic-platform` | 执行迁移时使用的非 root 用户 |
| 备份目录 | `/var/backups/basic-platform` | 仅保存数据库和本地文件备份 |
| 健康地址 | `http://127.0.0.1:8080` | API 的本机监听地址 |

应先按[前后端部署文档](前后端部署文档.md)完成 MySQL、运行用户、systemd、Nginx、环境文件和首次发布配置。

### 2.2 环境文件和数据库备份权限

`backup`、`migrate` 和 `deploy` 会读取 `MYSQL_HOST`、`MYSQL_PORT`、`MYSQL_DATABASE`、`MYSQL_USERNAME`、`MYSQL_PASSWORD`；文件备份会读取 `FILE_STORAGE_ROOT`。

为使 `mysqldump` 可以备份结构、数据、触发器、事件和存储过程，生产数据库账号或专用备份账号应拥有满足实际 MySQL 策略的只读、对象定义和例程读取权限。不要在文档或终端历史中直接写入数据库密码。

环境文件应限制为 root 和运行用户可读，例如：

```bash
sudo chown root:basic-platform /etc/basic-platform/basic-platform.env
sudo chmod 0640 /etc/basic-platform/basic-platform.env
```

> 如果企业密钥管理系统通过 systemd 环境变量注入配置，仍应提供脚本可读取的受控环境文件，或在执行脚本时使用 `--env-file` 指定仅供运维使用的安全配置文件。不要把密钥填入命令行参数。

### 2.3 发布源代码目录

`deploy` 会委托已有的 `scripts/bootstrap-linux.sh`，因此必须在**受控的项目源代码检出目录**执行。例如：

```bash
cd /srv/basic-platform/source
sudo bash scripts/ops-linux.sh deploy --release-id 20260723.1 --yes
```

若脚本不在源代码目录中执行，可显式指定受控源代码根目录：

```bash
sudo env BASIC_PLATFORM_SOURCE_ROOT=/srv/basic-platform/source \
  bash /srv/basic-platform/source/scripts/ops-linux.sh deploy --release-id 20260723.1 --yes
```

`BASIC_PLATFORM_SOURCE_ROOT` 必须指向包含 `scripts/bootstrap-linux.sh` 的真实项目根目录。发布前应先拉取并审查目标提交，不能让脚本从不受控来源下载或执行代码。

## 3. 可选配置覆盖

以下变量只用于运维脚本本身，不会修改应用的 `.env` 配置：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `BASIC_PLATFORM_SOURCE_ROOT` | 脚本所在仓库根目录 | `deploy` 查找 `scripts/bootstrap-linux.sh` 的源代码目录 |
| `BASIC_PLATFORM_API_SERVICE` | `basic-platform-api.service` | API systemd 单元名 |
| `BASIC_PLATFORM_WORKER_SERVICE` | `basic-platform-worker.service` | Worker systemd 单元名 |
| `BASIC_PLATFORM_RUN_USER` | `basic-platform` | 迁移程序的运行用户 |
| `BASIC_PLATFORM_DEPLOY_ROOT` | `/opt/basic-platform` | 发布根目录 |
| `BASIC_PLATFORM_BACKUP_DIR` | `/var/backups/basic-platform` | 备份目录 |
| `BASIC_PLATFORM_HEALTH_URL` | `http://127.0.0.1:8080` | 健康检查基础地址，不包含路径 |
| `BASIC_PLATFORM_OPS_LOG_DIRECTORY` | `/var/log/basic-platform` | 运维脚本自身日志目录 |

也可在单次执行时使用参数覆盖：`--env-file`、`--deploy-root`、`--backup-dir`、`--health-url`。

示例：

```bash
sudo env BASIC_PLATFORM_DEPLOY_ROOT=/srv/basic-platform \
  BASIC_PLATFORM_BACKUP_DIR=/srv/backup/basic-platform \
  bash scripts/ops-linux.sh status
```

## 4. 日常操作手册

以下命令均从项目根目录执行。会修改服务、数据库、发布目录或备份目录的命令必须确认，自动化场景必须显式传入 `--yes`。

### 4.1 上线前巡检

```bash
sudo bash scripts/ops-linux.sh doctor
sudo bash scripts/ops-linux.sh status
sudo bash scripts/ops-linux.sh health
```

`/healthz` 表示 API 进程可以响应；`/readyz` 还会验证依赖是否就绪。生产监控应以 `/readyz` 为主要可用性信号。

### 4.2 查看服务和日志

```bash
sudo bash scripts/ops-linux.sh status
sudo bash scripts/ops-linux.sh logs --lines 200
sudo bash scripts/ops-linux.sh logs --lines 200 --follow
```

### 4.3 服务启停

```bash
sudo bash scripts/ops-linux.sh restart --yes
sudo bash scripts/ops-linux.sh stop --yes
sudo bash scripts/ops-linux.sh start --yes
```

停止服务会中断正在处理的请求和 Worker 任务，不应作为常规故障排查的第一选择。

### 4.4 执行迁移

```bash
sudo bash scripts/ops-linux.sh migrate --yes
```

该操作先创建 MySQL 备份，再运行当前 `/opt/basic-platform/current/bin/migrate`。迁移前仍需确认数据库容量、锁影响、发布兼容性和恢复方案。

### 4.5 创建备份与校验

```bash
sudo bash scripts/ops-linux.sh backup --yes
sudo find /var/backups/basic-platform -type f -name '*.sha256' -print -exec sha256sum --check {} \;
```

备份文件默认分为：

```text
/var/backups/basic-platform/
├── mysql/
│   ├── <数据库名>-<UTC时间>.sql.gz
│   └── <数据库名>-<UTC时间>.sql.gz.sha256
└── uploads/
    ├── uploads-<UTC时间>.tar.gz
    └── uploads-<UTC时间>.tar.gz.sha256
```

备份完成后必须将该目录同步到独立、安全的异机备份或对象存储。环境文件、JWT 私钥、应用加密密钥和外部身份密钥须依据企业密钥管理制度单独加密备份。

### 4.6 受控发布

```bash
cd /srv/basic-platform/source
sudo bash scripts/ops-linux.sh deploy --release-id 20260723.1 --yes
```

默认发布前会做数据库备份，并交由 `scripts/bootstrap-linux.sh` 运行依赖安装、代码校验、构建、迁移、原子切换和服务重启。仅当产物源已由受控 CI 完整验证时，才可以使用：

```bash
sudo bash scripts/ops-linux.sh deploy --release-id 20260723.1 --skip-code-verify --yes
```

`--skip-code-verify` 不会跳过数据库迁移、备份或健康验证，不应在未审查的源代码上使用。

### 4.7 应用回退

```bash
sudo bash scripts/ops-linux.sh rollback --target 20260722.3 --yes
```

该命令仅回退 `/opt/basic-platform/current` 指向的应用版本并重启服务。若回退后的就绪检查失败，脚本会自动将软链接恢复为回退前版本；**不会回退 MySQL 数据库**。执行前必须由责任人确认目标版本与当前数据库结构兼容。

### 4.8 Nginx 配置重载

```bash
sudo bash scripts/ops-linux.sh reload-nginx --yes
```

脚本会先执行 `nginx -t`；配置校验失败时不会重载。

### 4.9 定期清理历史备份

```bash
sudo bash scripts/ops-linux.sh prune-backups --retain-days 14 --yes
```

该命令仅删除本脚本在 `mysql/` 和 `uploads/` 目录内生成的 `.sql.gz`、`.tar.gz` 与对应 `.sha256` 文件。保留期应根据企业 RPO、RTO 和异机备份策略确定，生产环境不建议少于 14 天。

## 5. 自动化调度建议

脚本提供 `--yes` 以便交给受控 CI 或 systemd timer 调用，但自动化前必须先在预发布环境验证。推荐将备份和只读巡检分开调度：

```ini
# /etc/systemd/system/basic-platform-backup.service
[Unit]
Description=Basic Platform daily backup

[Service]
Type=oneshot
ExecStart=/usr/bin/bash /srv/basic-platform/source/scripts/ops-linux.sh backup --yes
```

```ini
# /etc/systemd/system/basic-platform-backup.timer
[Unit]
Description=Run Basic Platform daily backup

[Timer]
OnCalendar=*-*-* 02:30:00
Persistent=true

[Install]
WantedBy=timers.target
```

启用前应根据实际源代码目录修改大小写完全一致的 `ExecStart` 路径：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now basic-platform-backup.timer
sudo systemctl list-timers --all | grep basic-platform-backup
```

`doctor` 适合每日巡检，`health` 适合分钟级监控；二者均不修改应用状态。备份成功日志不等于异机同步成功，应由备份平台或监控系统针对备份文件、校验结果和异机副本建立告警。

## 6. 故障处理原则

1. 先执行 `status`、`health` 和 `logs` 收集事实，再决定是否重启；不要只因单个请求失败直接重启。
2. `/healthz` 成功但 `/readyz` 失败时，优先检查 MySQL、文件目录权限、环境文件和依赖状态。
3. 发布后就绪失败时，不要立即盲目回退；先确认迁移是否已执行，以及旧版本是否兼容当前数据库。
4. 数据库恢复必须在隔离环境演练后执行，并同时考虑 `FILE_STORAGE_ROOT`、密钥和对应应用版本。
5. 发现密码、Token、授权码或私钥进入日志时，应按安全事件处理：先限制访问与轮换密钥，再保留证据并排查来源。

## 7. 验收清单

在测试或预发布 Linux 主机完成以下验证后，再用于生产：

```bash
sudo bash scripts/ops-linux.sh --help
sudo bash scripts/ops-linux.sh doctor
sudo bash scripts/ops-linux.sh backup --yes
sudo bash scripts/ops-linux.sh migrate --yes
sudo bash scripts/ops-linux.sh restart --yes
sudo bash scripts/ops-linux.sh health
sudo bash scripts/ops-linux.sh logs --lines 50
```

还必须人工确认：

- MySQL 备份可在隔离环境恢复；
- 上传文件备份可恢复，且数据库元数据与文件恢复点一致；
- 发布与回退前后的 API、Worker、登录、MFA、权限、审计、文件功能可用；
- 备份文件已同步到独立存储；
- 环境文件与私钥没有出现在备份、日志、终端记录或版本库中。

## 8. 关联文档

- [前后端部署文档](前后端部署文档.md)
- [服务器上线检查清单](服务器上线检查清单.md)
- [Linux 环境自动安装脚本说明](../04_后端开发/Linux环境自动安装脚本说明.md)
- [项目说明](../项目说明.md)
