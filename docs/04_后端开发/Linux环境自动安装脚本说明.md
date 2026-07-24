# Linux 环境检查与发布脚本说明

## 1. 目的

项目根目录的 `scripts/bootstrap-linux.sh` 同时提供 Linux 开发环境初始化、环境检查、代码验证和单机原生发布能力。

它可以完成以下已实现的工作：

1. 读取 `/etc/os-release`，识别 Linux 发行版、内核、CPU 架构和软件包管理器；
2. 安装编译工具、Git、Make、OpenSSL、Curl、证书和压缩工具等基础依赖；
3. 从 `backend/go.mod` 读取 Go 最低版本，安装经 SHA-256 校验的官方 Go；
4. 安装满足前端 Vite 要求的 Node.js 与 npm，并校验官方发行包 SHA-256；
5. 可选安装本机 Oracle MySQL、创建本地开发数据库及应用账号；
6. 从 `.env.example` 创建或补齐开发用 `.env`、数据目录和 Ed25519 JWT 密钥；
7. 执行 `go mod download`、`npm ci`、数据库迁移、Go 检查/测试及前端测试；
8. 在首次 `--deploy` 时创建专用运行用户和用户组、生产环境文件目录与模板、运行目录、Ed25519 JWT 密钥对，并写入脚本托管的 API、Worker systemd 单元；
9. 在 `--deploy` 模式下构建 API、Worker、迁移程序和前端静态资源，执行迁移，原子切换发布版本；
10. 在指定 `--restart-services` 时启用并重启 API、Worker systemd 服务；
11. 在 `--stop` 模式下停止并禁用已部署的 API、Worker systemd 服务；
12. 在 `--uninstall` 模式下清理本应用的发布文件、服务单元、运行文件与可选数据库资源；
13. 对失败步骤输出退出码、脚本行号、失败命令、处理建议和完整日志位置。

首次原生发布仅自动创建能够从项目确定的操作系统资源。脚本不会猜测或写入生产域名、MySQL 地址和密码、四个 IAM 加密密钥，也不会配置 Nginx/TLS、数据库备份、密钥轮换或防火墙。若环境文件首次生成，脚本会在完成前置资源创建后**停止**；运维人员替换全部 `REPLACE_WITH_*` 值后，必须重新执行同一条发布命令。`IAM_BOOTSTRAP_TOKEN` 默认留空，因此首次超级管理员接口默认关闭；仅在受控初始化时临时设置该令牌。

## 2. 支持范围

### 2.1 Linux、架构与数据库

- CPU：`x86_64/amd64`、`aarch64/arm64`；
- 软件包管理器：APT、DNF、YUM、Zypper、Pacman、APK；
- 本机 Oracle MySQL 自动安装：APT 中能够提供非 MariaDB `mysql-server` 的系统，以及仓库已经提供 `mysql-server` 的 DNF/YUM 系统；
- Debian、Alpine、Arch、openSUSE 等默认仓库可能缺少 Oracle MySQL 或提供 MariaDB。脚本不会静默用 MariaDB 替代项目要求的 MySQL。

脚本会拒绝 macOS、Windows、无法读取 `/etc/os-release` 的主机和未支持的 CPU 架构。

### 2.2 版本来源

- Go 版本从 `backend/go.mod` 的 `go` 指令读取；
- Node.js 默认版本为 `22.17.0`，可用 `NODE_VERSION` 覆盖，但必须满足当前前端 Vite 的 Node.js 版本要求；
- 数据库目标为 Oracle MySQL 8.x；检测到 MariaDB 时，脚本会停止数据库初始化并给出提示。

## 3. 常用命令

所有命令均从项目根目录执行。

### 3.1 仅检查环境

```bash
bash scripts/bootstrap-linux.sh --check
```

检查系统、工具链、环境文件、数据库连接和项目依赖，不修改系统。若服务器使用远程 MySQL 且未安装 `mysql` 客户端，可额外加 `--skip-mysql-server`；这会跳过客户端存在性要求，但实际迁移仍会通过应用连接验证数据库。

### 3.2 初始化开发环境

```bash
bash scripts/bootstrap-linux.sh --bootstrap --yes
```

该模式可安装依赖、创建/补齐开发 `.env`、初始化本机 MySQL、安装项目依赖、执行迁移和代码验证。它仅适用于开发或受控验收环境。

使用远程 MySQL 时：

```bash
bash scripts/bootstrap-linux.sh \
  --bootstrap \
  --yes \
  --skip-mysql-server \
  --skip-database-init
```

脚本不会在远程数据库创建账号。若未添加 `--skip-migration`，环境文件中的应用账号必须具有当前迁移所需的数据库权限。

### 3.3 分阶段执行

```bash
# 安装系统依赖、Go、Node.js 和本机 MySQL
bash scripts/bootstrap-linux.sh --install-system --yes

# 创建或补齐开发 .env、数据目录和开发 JWT 密钥
bash scripts/bootstrap-linux.sh --configure

# 安装 Go Module 与前端 npm 依赖
bash scripts/bootstrap-linux.sh --install-project

# 执行 Go 格式检查、静态检查、测试和前端测试
bash scripts/bootstrap-linux.sh --verify
```

### 3.4 集成前后端发布

以下命令适用于已完成 Nginx/TLS 与数据库备份准备的 Linux 服务器。首次发布必须使用 `sudo bash`：脚本会创建默认的 `basic-platform` 运行用户/组、`/etc/basic-platform`、`/var/lib/basic-platform`、`/var/log/basic-platform`、JWT 密钥和 systemd 单元。环境文件不存在时，首次执行会生成生产模板并安全结束；填写模板中的全部 `REPLACE_WITH_*` 值后，重新执行同一条命令才会构建和发布。`IAM_BOOTSTRAP_TOKEN` 保持为空即可；仅在受控创建首个超级管理员前临时设置。

CentOS 7 默认的 OpenSSL 1.0.2 不支持 `ED25519`，因此不能只依赖 `openssl genpkey -algorithm ED25519`。脚本会先尝试 OpenSSL；若未安装 OpenSSL 或该版本不支持 Ed25519，则调用随项目提供的 `scripts/generate-ed25519-jwt-key-pair.go`，使用 Go 标准库生成后端可读取的 PKCS#8 私钥和 PKIX 公钥 PEM 文件。生产发布本身已经要求安装项目所需的 Go 版本；若系统未安装 Go，脚本会明确失败而不会写入半成品密钥。

```bash
sudo bash scripts/bootstrap-linux.sh \
  --deploy \
  --yes \
  --env-file /etc/basic-platform/basic-platform.env \
  --deploy-root /opt/basic-platform \
  --restart-services
```

若生产数据库为远程 MySQL 且服务器未安装 `mysql` 客户端，追加 `--skip-mysql-server`：

```bash
sudo bash scripts/bootstrap-linux.sh \
  --deploy \
  --yes \
  --skip-mysql-server \
  --env-file /etc/basic-platform/basic-platform.env \
  --deploy-root /opt/basic-platform \
  --restart-services
```

`--deploy` 的执行顺序如下：

1. 校验以 root 身份执行，创建或复用 `basic-platform` 运行用户和用户组；
2. 创建环境文件父目录；若 `/etc/basic-platform/basic-platform.env` 不存在，创建权限为 `0640` 的生产模板、`/opt/basic-platform/releases` 发布目录、运行目录、JWT 密钥和 systemd 单元，然后停止，要求填写全部 `REPLACE_WITH_*` 占位符后重新执行；`IAM_BOOTSTRAP_TOKEN` 默认留空，不属于必填占位符；
3. 创建或复用 JWT 密钥对，设置运行目录和环境文件权限，并以运行用户验证读取/写入权限。脚本只会清理长度为 0 的失败残留密钥文件，不会覆盖任何非空既有密钥；若 OpenSSL 不支持 Ed25519，会回退到 Go 标准库生成器；
4. 创建或更新带 `# Managed by Basic Platform bootstrap-linux.sh` 标记的 API、Worker systemd 单元；若同名单元由运维自行维护，脚本保留原文件不覆盖；
5. 在构建、迁移和启动服务之前，检查已有生产环境文件的 `APP_ENV=production`、必填项和未替换的 `REPLACE_WITH_*` 占位符。这样可在配置尚未填完时先修复运行用户、目录、密钥和 systemd 前置资源，但不会发布或启动应用；
6. 安装 Go/npm 项目依赖并执行环境检查；默认执行 `gofmt`、`go mod verify`、`go vet ./...`、`go test ./...` 和前端测试；
7. 在 `releases/.staging.*` 中构建 `api`、`worker`、`migrate` 三个后端二进制和 `frontend/dist`，写入 `release-info`；
8. 将暂存目录固化为正式版本，默认执行该版本自带的 `migrate` 程序，然后原子更新 `current` 软链接；
9. 指定 `--restart-services` 时启用并重启 `basic-platform-api.service` 与 `basic-platform-worker.service`，并检查服务是否处于 active；
10. 清理超出保留数量的旧版本。

发布目录布局如下：

```text
/opt/basic-platform/
├── current -> releases/<release-id>
└── releases/
    └── <release-id>/
        ├── bin/
        │   ├── api
        │   ├── worker
        │   └── migrate
        ├── frontend/
        └── release-info
```

环境文件不会复制到发布目录。`ENV_FILE` 必须继续指向 `/etc/basic-platform/basic-platform.env` 这类受权限保护的外部文件；JWT 私钥、应用加密密钥、数据库密码、上传文件和日志也不应写入版本目录。

> **迁移注意事项**：切换软链接以前的构建或迁移失败，不会替换当前应用版本；但数据库迁移一旦执行，不会被脚本自动回滚。每次生产发布前必须完成 MySQL 和上传/归档文件备份，并确认旧版本兼容新数据库结构后再考虑应用回退。


### 3.5 关闭应用系统

仅临时关闭 API 与 Worker、保留发布版本、环境文件、数据库、上传文件、日志以及 Nginx 配置时，执行：

```bash
sudo bash scripts/bootstrap-linux.sh \
  --stop \
  --yes
```

该命令会对 `basic-platform-api.service` 和 `basic-platform-worker.service` 执行停止并取消开机自启。它不停止共享的 MySQL、Nginx、Go 或 Node.js，也不删除任何项目数据。

> **前端可见性说明**：Vue 前端是由 Nginx 提供的静态文件。`--stop` 后，Nginx 仍可能返回登录页或缓存的静态页面，但页面调用 API 会失败。若要使外部入口也下线，应在确认该虚拟主机只属于本应用后，使用下面卸载命令中的 `--nginx-site` 删除该应用的 Nginx 站点配置；脚本不会停掉或删除可能被其他站点共用的 Nginx 服务。

### 3.6 卸载应用系统

先执行数据库和文件备份。若仅删除服务器上的 Basic Platform 应用资源，而保留数据库，使用：

```bash
sudo bash scripts/bootstrap-linux.sh \
  --uninstall \
  --yes \
  --env-file /etc/basic-platform/basic-platform.env \
  --deploy-root /opt/basic-platform
```

上述命令会依次停止并禁用 API、Worker 服务，删除 `/opt/basic-platform` 发布目录、下列 systemd 单元文件、环境文件，以及环境文件中明确配置的运行路径：

- `/etc/systemd/system/basic-platform-api.service`；
- `/etc/systemd/system/basic-platform-worker.service`；
- `FILE_STORAGE_ROOT`（上传/文件存储目录）；
- `LOG_DIRECTORY`（应用日志目录）；
- `AUTH_JWT_PRIVATE_KEY_PATH`、`AUTH_JWT_PUBLIC_KEY_PATH` 指向的 JWT 密钥文件；
- `--env-file` 指定的环境文件（不会删除 `.env.example` 模板）。

若目标是**删除该项目在服务器中的全部可识别业务数据**，且已确认数据库、Nginx 虚拟主机和运行用户均专用于本项目，可追加显式清理参数：

```bash
sudo env MYSQL_ADMIN_PASSWORD='数据库管理员密码' \
  bash scripts/bootstrap-linux.sh \
  --uninstall \
  --yes \
  --env-file /etc/basic-platform/basic-platform.env \
  --deploy-root /opt/basic-platform \
  --purge-database \
  --nginx-site /etc/nginx/conf.d/basic-platform.conf \
  --remove-system-user basic-platform
```

参数含义与限制：

- `--purge-database` 删除环境文件中 `MYSQL_DATABASE` 指定的库。对于 `MYSQL_HOST=127.0.0.1` 或 `localhost`，还会删除脚本初始化时创建的 `MYSQL_USERNAME@127.0.0.1` 与 `MYSQL_USERNAME@localhost` 账号；远程 MySQL 的账号 Host 范围无法安全推断，必须由数据库管理员按实际账号另行删除。
- `--nginx-site` 只删除**明确传入**的 Nginx 虚拟主机配置，然后执行 `nginx -t` 并尝试重载 Nginx。不要传入共享站点配置或 Nginx 主配置；脚本仅接受 `/etc/nginx/` 或 `/usr/local/nginx/conf/` 下的路径。
- `--remove-system-user` 只删除**明确传入**的专用 Linux 运行用户，并且仅在同名用户组为空时删除该用户组。
- 脚本不会删除 MySQL/Nginx 服务本身、操作系统工具链、Docker、其他项目的数据或共享系统用户。这些资源可能被其他应用使用，不应以“卸载本项目”为由自动删除。

所有卸载路径必须为绝对路径；脚本拒绝删除 `/`、`/opt`、`/etc`、`/var` 等过宽目录。`--uninstall` 是不可逆操作，建议先使用 `--dry-run` 核对动作范围：

```bash
sudo bash scripts/bootstrap-linux.sh \
  --uninstall \
  --dry-run \
  --yes \
  --env-file /etc/basic-platform/basic-platform.env \
  --deploy-root /opt/basic-platform \
  --purge-database \
  --nginx-site /etc/nginx/conf.d/basic-platform.conf \
  --remove-system-user basic-platform
```

## 4. 参数说明

| 参数 | 作用 |
|---|---|
| `--env-file PATH` | 指定环境文件，默认项目根目录 `.env` |
| `--yes` | 非交互确认系统安装、数据库初始化或发布 |
| `--dry-run` | 只输出计划，不执行实际修改 |
| `--verbose` | 将执行命令的完整输出同时显示在终端和日志 |
| `--skip-mysql-server` | 不安装本机 MySQL；也可在远程 MySQL 场景放宽客户端检查 |
| `--skip-database-init` | 不创建数据库及应用账号 |
| `--skip-migration` | 不执行数据库迁移；仅当确认没有待执行迁移时使用 |
| `--skip-project-deps` | 不执行 `go mod download` 和 `npm ci`；`--deploy` 不允许使用此参数 |
| `--deploy-root PATH` | 发布根目录，默认项目根目录 `.deploy`；生产建议 `/opt/basic-platform` |
| `--release-id ID` | 指定发布版本号，只允许字母、数字、点、下划线、短横线 |
| `--keep-releases N` | 成功发布后保留的版本总数，默认 `5`，最小 `1` |
| `--restart-services` | 发布切换后启用并重启 API 与 Worker systemd 服务；首次 `--deploy` 会在没有同名自定义单元时创建脚本托管单元 |
| `--purge-database` | 仅可与 `--uninstall` 配合；删除环境文件指定的 MySQL 数据库，本机库同时清理脚本创建的两个应用账号 |
| `--nginx-site PATH` | 仅可与 `--uninstall` 配合；删除明确指定的本应用 Nginx 虚拟主机配置；可重复传入 |
| `--remove-system-user USER` | 仅可与 `--uninstall` 配合；删除明确指定的专用运行用户及其同名空用户组 |
| `--skip-code-verify` | 发布前跳过 Go/前端代码验证；仅适用于已经由受控 CI 验证的源码 |

可选环境变量：

```bash
export NODE_VERSION=22.17.0
export MYSQL_ADMIN_USERNAME=root
export MYSQL_ADMIN_PASSWORD='仅在不能使用本机 socket 管理时设置'
export BASIC_PLATFORM_BOOTSTRAP_LOG=/tmp/basic-platform-bootstrap.log
export BASIC_PLATFORM_DEPLOY_ROOT=/opt/basic-platform
export BASIC_PLATFORM_RUNTIME_USER=basic-platform
export BASIC_PLATFORM_RUNTIME_GROUP=basic-platform
```

不要将 `MYSQL_ADMIN_PASSWORD`、`.env`、JWT 私钥或脚本日志中的敏感运行信息提交到 Git。

## 5. 发布前置条件与安全边界

1. 首次原生发布必须以 root 身份运行；脚本会创建或复用 `basic-platform` 用户/组，默认可用 `BASIC_PLATFORM_RUNTIME_USER`、`BASIC_PLATFORM_RUNTIME_GROUP` 覆盖；
2. Nginx 的静态文件根目录仍须由运维配置为 `/opt/basic-platform/current/frontend`，以便与后端随同一个 `current` 软链接切换；
3. 环境文件不存在时，脚本创建 `/etc/basic-platform/basic-platform.env` 生产模板、`/opt/basic-platform/releases` 发布目录、JWT 密钥、上传目录、日志目录和托管 systemd 单元，但会停止发布；必须填写全部 `REPLACE_WITH_*` 占位符并重新执行；`IAM_BOOTSTRAP_TOKEN` 默认留空，首次初始化管理员时再临时设置。CentOS 7 等不支持 Ed25519 的旧版 OpenSSL 主机自动改用 Go 标准库密钥生成器；
4. 脚本只删除长度为 0 的 JWT 密钥生成失败残留，不会覆盖非空密钥；若只存在一个非空密钥文件，会停止并要求人工核对；
5. 脚本会验证运行用户可读取环境文件与 JWT 密钥，且可写上传和日志目录；
6. 已存在且不带脚本托管标记的同名单元不会被覆盖；运维自定义单元必须自行确保 `WorkingDirectory` 和 `ExecStart` 指向 `/opt/basic-platform/current`；
7. 发布产物只包含二进制、前端静态文件和非敏感元数据；脚本会将发布目录、`bin` 和前端目录设置为可遍历/读取的常规权限，运行密钥不应存放在其中；
8. `.env` 已存在时，开发配置模式保留已有非空值；生产发布不会替换已有环境文件或 IAM 加密密钥；
9. 脚本不安装 Redis，不引入消息队列，不执行邮件或短信相关安装；
10. 卸载时仅删除明确识别为本应用资源的绝对路径，不会自动删除共享的 MySQL、Nginx、工具链或未明确指定的用户、站点配置。

## 6. 不自动执行的事项

- 不安装或配置 Nginx、TLS 证书、生产域名；
- 不修改防火墙、SELinux、AppArmor 或公网端口；
- 不创建生产数据库、不生成 MySQL 密码或四个 IAM 加密密钥、不执行数据库/文件备份；
- 不覆盖已有生产环境文件、已有 JWT 密钥对或运维自定义的同名 systemd 单元，不管理密钥轮换；
- 不在发布失败时自动进行数据库 down migration 或数据库恢复；
- 不做登录、权限、审计、文件、OIDC/钉钉回调等业务冒烟验收。

## 7. 错误处理与排查

脚本启用 Bash 严格模式。失败时会输出当前步骤、退出码、脚本行号、失败命令、处理建议、最近日志和完整日志路径。默认日志在：

```text
/tmp/basic-platform-bootstrap-YYYYMMDD-HHMMSS-PID.log
```

建议按以下顺序排查：

1. 以日志中的第一个失败命令为准，而不是最后一个连带错误；
2. 依赖下载失败时检查 DNS、软件源、代理、证书、`go.dev`、Node/npm registry 连通性；
3. 数据库或迁移失败时检查环境文件、账号权限、网络和迁移校验和；不要修改已经执行的历史迁移；
4. 服务重启失败时查看 `journalctl -u basic-platform-api.service` 与 `journalctl -u basic-platform-worker.service`；
5. 前端无法访问时检查 Nginx 根目录是否为 `/opt/basic-platform/current/frontend`，并执行 `nginx -t`；
6. 修复后使用相同命令重新发布；不要手动删除当前版本或发布目录中的环境文件。
