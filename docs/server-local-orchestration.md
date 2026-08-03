# 在远程服务器上运行本地统一编排（测试/演示服务器）

> 更新日期：2026-08-03。
> 适用场景：一台 **测试/演示服务器**（例如阿里云 ECS `47.111.20.119`），希望像本地开发一样使用
> `docker-local.sh` 全家桶，并让 UI 的"一键接入 / 新增接入"可用。
> 生产环境（`platform/deploy/production/`、CI/CD 发布不可变镜像）**不要**用本文档的编排方式。

## 0. 先说清楚一个事实

测试/演示环境的一键接入依赖 **本地部署 Agent + Docker Socket + 完整工作区**。正式生产目录也提供独立的受控 Agent，但只允许内置 `contract_management/prod`、不可变镜像和固定 Compose 服务；本文仍只介绍把服务器当**本地开发机**的方式：

```text
一个 frontend 容器（四个前端模块）
api / contract-api / customer-api / portal-api 四个独立后端容器
MySQL x4、Temporal、迁移与部署 Agent
仅 frontend 对外发布 8081，其余全部在内网
```

## 1. 服务器准备

```bash
# CentOS/RHEL
sudo dnf install -y git openssl
sudo systemctl enable --now docker
sudo systemctl status docker

# 或 Debian/Ubuntu
sudo apt-get update && sudo apt-get install -y git openssl
sudo systemctl enable --now docker
```

防火墙/安全组开放 `8081`（ECS 安全组 + 本机防火墙都要放行）：

```bash
# firewalld
sudo firewall-cmd --permanent --add-port=8081/tcp && sudo firewall-cmd --reload
# 或 ufw
sudo ufw allow 8081/tcp
```

## 2. 把工作区放到服务器

需要 **同一个父目录下** 的四个仓库（目录结构必须和本地一致）：

```bash
# 例如放到 /opt/workspace，先建目录
sudo mkdir -p /opt/workspace && sudo chown "$(id -u):$(id -g)" /opt/workspace
cd /opt/workspace

# 方式一：直接拷贝本地工作区（推荐，保证一致）
rsync -a --exclude node_modules --exclude .git \
  /path/to/Unified_Identity_Authentication_Platform/ /opt/workspace/Unified_Identity_Authentication_Platform/

# 方式二：分别 clone 四个仓库（需要四个仓库都可访问）
git clone <platform-git>        Unified_Identity_Authentication_Platform/platform
git clone <frontend-git>        Unified_Identity_Authentication_Platform/frontend
git clone <contract-git>        Unified_Identity_Authentication_Platform/contract_management
git clone <customer-git>        Unified_Identity_Authentication_Platform/customer_and_opportunity
```

> 四个仓库缺一不可：`docker-local.sh` 会从 `platform`、`frontend`、`../contract_management`、
> `../customer_and_opportunity` 构建镜像，找不到任何一个都会构建失败。

## 3. 首次启动（会生成环境文件并初始化管理员）

```bash
cd /opt/workspace/Unified_Identity_Authentication_Platform/platform

# 交互式（会提示输入管理员显示名/账号/密码）
bash scripts/docker-local.sh up

# 或 CI/非交互（密码走 stdin，不要用 --admin-password）
printf '%s\n' '你的管理员密码' | bash scripts/docker-local.sh up \
  --admin-display-name '平台管理员' \
  --admin-account-name admins \
  --admin-password-stdin
```

首次启动自动完成：

- 从模板生成 `docker/.env.local`、`../contract_management/.env.local`、
  `docker/.env.customer.local`、`docker/.env.portal.local`（含随机密码/密钥，权限 0600）；
- 串行拉取基础镜像、构建 1 个前端 + 4 个后端镜像；
- 执行数据库迁移、初始化第一个超级管理员、分阶段启动并做网关自检。

## 4. 配置公网/远程访问（服务器是 IP + HTTP）

默认 `docker-local.sh` 把公开地址写成 `http://localhost:8081`，且前端只绑定 `127.0.0.1`。
要让外部用 `http://47.111.20.119:8081` 访问，启用"局域网覆盖"模式（对公网 IP 同样适用）：

```bash
# 1) 允许 OAuth HTTP 回调（公网 IP 非回环，默认被平台拒绝）
sed -i 's/^AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=.*/AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=true/' docker/.env.local

# 2) 把公开地址切到服务器 IP，前端绑定 0.0.0.0
bash scripts/lan-access.sh enable --address 47.111.20.119 --port 8081

# 3) 重建 API（让上面两步的配置生效）并重新拉起
bash scripts/docker-local.sh up
```

> `lan-access.sh enable` 会把以下内容写入 `docker/.env.lan` 等覆盖文件并重建相关容器：
> `APP_PUBLIC_BASE_URL/OIDC_ISSUER/OIDC_REDIRECT_URI=.../contract_management/auth/callback`、
> `.../customer-opportunity/auth/callback`，并把 `FRONTEND_BIND_ADDRESS` 切到 `0.0.0.0`。
> 该模式使用 HTTP，**只适用于可信网络/测试机**，不要用于公网生产。

## 5. 验证

```bash
cd /opt/workspace/Unified_Identity_Authentication_Platform/platform
bash scripts/docker-local.sh verify          # 网关自检
bash scripts/docker-local.sh ps             # 看所有容器
curl -I http://47.111.20.119:8081/          # 外部可访问
```

浏览器打开 `http://47.111.20.119:8081/` → 用平台管理员登录 →
**应用接入 → 合同管理系统 → 新增接入环境**（或直接"新增接入"）即可一键接入，和本地行为一致。

服务器 shell 里想用脚本接入时，用回环地址（脚本只允许 HTTPS 或回环 HTTP）：

```bash
bash scripts/subsystem.sh onboard \
  --preset contract-management-local \
  --api-base-url http://127.0.0.1:8081/api/v1 \
  --platform-origin http://47.111.20.119:8081 \
  --account admins
```

## 6. 环境文件与备份

需要备份（全部含 Secret，保持 0600）：

```text
platform/docker/.env.local
platform/docker/.env.customer.local
platform/docker/.env.portal.local
platform/docker/.env.lan            （公网覆盖，lan-access enable 生成）
contract_management/.env.local
platform/data/keys/                 （JWT 私钥，丢了登录全部失效）
platform/data/uploads/
```

```bash
cd /opt/workspace/Unified_Identity_Authentication_Platform
sudo install -d -m 700 /opt/backup/basic-platform
tar -czf /opt/backup/basic-platform/env-and-keys-$(date +%F).tgz \
  platform/docker/.env.local platform/docker/.env.customer.local \
  platform/docker/.env.portal.local platform/docker/.env.lan \
  contract_management/.env.local platform/data/keys platform/data/uploads
```

数据库备份示例：

```bash
cd platform
MYSQL_ROOT_PASSWORD="$(awk -F= '$1=="MYSQL_ROOT_PASSWORD"{print substr($0,index($0,"=")+1)}' docker/.env.local)"
docker exec basic-platform-local-mysql-1 sh -lc \
  "MYSQL_PWD='$MYSQL_ROOT_PASSWORD' mysqldump -uroot --single-transaction basic_platform" \
  > /opt/backup/basic-platform/basic_platform.sql
```

## 7. 开机自启与保活

Docker 本身开机自启后，容器 `restart: unless-stopped` 会自动恢复，通常无需额外配置：

```bash
sudo systemctl enable --now docker
```

如需"开机后确保编排完整拉起"（例如停机维护后），加一个 systemd oneshot：

```ini
# /etc/systemd/system/basic-platform-local.service
[Unit]
Description=Basic Platform local orchestration
After=docker.service network-online.target
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/opt/workspace/Unified_Identity_Authentication_Platform/platform
ExecStart=/usr/bin/env bash scripts/docker-local.sh up
ExecStartPost=/usr/bin/env bash scripts/docker-local.sh verify

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now basic-platform-local
```

## 8. 日常维护

```bash
cd /opt/workspace/Unified_Identity_Authentication_Platform/platform

bash scripts/docker-local.sh ps                    # 状态
bash scripts/docker-local.sh logs api contract-api # 看日志
bash scripts/docker-local.sh verify               # 网关自检
bash scripts/docker-local.sh refresh-api          # 只重建平台后端
bash scripts/docker-local.sh refresh-frontend     # 只重建统一前端
bash scripts/docker-local.sh refresh-contract-api # 只重建合同后端
```

升级（拉代码 + 重建）：

```bash
cd /opt/workspace/Unified_Identity_Authentication_Platform
for d in platform frontend contract_management customer_and_opportunity; do
  git -C "$d" pull --ff-only
done
cd platform && bash scripts/docker-local.sh up --build
```

> 升级不会覆盖已有的 `.env.local` 等环境文件，也不会删除 Application / OAuth Client / 登录目标。

## 9. 常见问题

| 现象 | 处理 |
| --- | --- |
| UI 一键接入 503"未启用自动部署 Agent" | 本地确认 `compose.local.yaml` 已启动 `subsystem-provisioner`；生产确认已同步最新 `deploy/production` 资产并且 `platform-api`、`subsystem-provisioner` 健康 |
| 登录回调被拒 / unauthorized_client | `AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=true` 没生效；改后需重建 api（`docker-local.sh up`） |
| 外部访问 8081 不通 | 检查 ECS 安全组 + 本机防火墙 + `FRONTEND_BIND_ADDRESS=0.0.0.0`（lan-access enable 后应已设置） |
| 平台账号提示已有有效会话 | 使用 `--replace-existing-session` 或先在原终端退出 |
| 构建拉镜像超时 | `docker-local.sh up` 已串行重试基础镜像；网络差可手动 `docker pull` 后再跑 |

## 10. 安全提醒（务必阅读）

- 本文档方式把平台管理后台 + 各子系统以 **HTTP 明文** 暴露在你指定的地址上；**只允许内网/测试环境使用**。
- 若该地址公网可达，任何人可访问登录页并尝试口令爆破；请至少加 IP 白名单、改强口令，或直接套一层 HTTPS 反向代理（Nginx/CLB 终止 TLS，内部仍可走 8081）。
- 测试数据不要与生产数据混用；`.env.local` 中的密码/密钥与生产 `.env` 相互独立，不要互相复制。
- 正式生产请使用 `platform/deploy/production/` + CI/CD 发布，并按 `production/README.md` 的控制面流程接入子系统。
