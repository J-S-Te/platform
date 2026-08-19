# 本地 Docker 与脚本使用说明

> 更新日期：2026-08-19

## 1. 推荐入口

在 `platform` 目录执行：

```bash
bash scripts/docker-local.sh up
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh verify
bash scripts/docker-local.sh logs api subsystem-provisioner
```

`docker-local.sh up`：

- 只在缺失时从模板创建基础平台、合同管理、客户与商机管理及客户自助门户环境文件；
- 不要求重建已有 `.env.local`；
- 构建一个统一前端镜像，以及基础平台、合同管理、客户与商机管理、客户自助门户的独立 API/Worker 镜像；客户自助门户完成接入前只构建镜像、不启动 `portal-api`；
- 执行迁移，按需初始化管理员，再分阶段启动服务；
- 不删除已存在的 Application、Environment、LoginTarget 或 OAuth Client。

## 2. 首次管理员

人工运行时直接交互输入。CI 推荐：

```bash
printf '%s\n' "$ADMIN_PASSWORD" | bash scripts/docker-local.sh up \
  --admin-display-name '平台管理员' \
  --admin-account-name admins \
  --admin-password-stdin
```

`--admin-password PASSWORD` 仅为旧调用兼容保留，会把密码暴露到 shell history/进程参数，不应继续使用。

## 3. 环境文件

| 文件 | 用途 | 是否自动覆盖 |
| --- | --- | --- |
| `docker/.env.local.example` | 基础平台本地模板 | 否 |
| `docker/.env.local` | 本地实际配置，含 Secret | 只在缺失时创建 |
| `docker/.env.customer.local.example` | 客户与商机管理本地模板 | 否 |
| `docker/.env.customer.local` | 客户与商机管理实际配置，含数据库密码和密钥 | 只在缺失时创建 |
| `docker/.env.portal.local.example` | 客户自助门户本地模板 | 否 |
| `docker/.env.portal.local` | 客户自助门户实际配置；接入前只有本地密钥，接入 Agent 写入 OIDC 和六组最小权限服务凭据 | 缺失时创建，接入时受控更新 |
| `../<subsystem>/.env.example` | 子系统模板 | 否 |
| `../<subsystem>/.env.local` | 子系统实际配置 | onboard 时受控更新 |
| `docker/.env.lan` | 临时局域网覆盖 | `lan-access.sh` 管理 |

所有实际 Secret 文件必须保持 `0600`，禁止提交。

当前只保留两套 Docker 配置边界：本地/测试使用根目录 `compose.local.yaml`，生产使用 `deploy/production/compose.yaml`。旧的根目录 `compose.yaml`、`docker/.env` 和 `prepare-docker-env.sh` 已删除，禁止重新引入第三套并行配置。仓库根 `.env` 只允许由开发者从 `.env.example` 本地创建，不得提交。

## 4. 局域网访问

```bash
bash scripts/lan-access.sh enable --address 192.168.3.11 --port 8081
bash scripts/lan-access.sh status
bash scripts/lan-access.sh disable
```

enable 会生成权限为 `0600` 的临时覆盖文件并重建统一 frontend、基础平台 API、合同 API 和 CRM API；若客户自助门户已接入，也会重建独立 Portal API。它保留数据库和统一登录控制面记录。该模式使用 HTTP，只适用于可信局域网。

局域网 OAuth HTTP 回调与接入脚本访问平台 API 是两套策略，详见 [子系统开发与统一身份接入手册](./subsystem-onboarding.md#33-http-回调策略)。

## 5. 定向更新

```bash
bash scripts/docker-local.sh refresh-api
bash scripts/docker-local.sh refresh-frontend
bash scripts/docker-local.sh refresh-contract-api
bash scripts/docker-local.sh refresh-customer-api
bash scripts/docker-local.sh start-presale-alert-worker
bash scripts/docker-local.sh refresh-portal-api
```

定向更新不会重新接入子系统：

- `refresh-api`：迁移并重建基础平台 API/provisioner；
- `refresh-frontend`：同时重建基础平台、合同管理、客户与商机管理和客户自助门户前端；
- `refresh-contract-api`：合同迁移并重建合同后端。
- `refresh-customer-api`：执行客户与商机 CRM 迁移并重建独立 `customer-api` 和 `customer-presale-alert-worker`；同时刷新统一前端网关配置。
- `start-presale-alert-worker`：只构建并启动客户与商机 TS-008 售前预警 Worker，适合 Worker 单独更新或故障恢复。
- `refresh-portal-api`：仅在 `customer_portal/dev` 已接入后执行 Portal 迁移并重建独立 `portal-api`。
- 未接入时统一前端中的 `/customer-portal/` 静态页面已存在，但 API 健康检查会失败；这是“尚未接入”的预期状态，不应伪装为可用门户。

## 6. 业务容器拓扑

| Compose 服务 | Docker 镜像 | 内容 | 宿主机端口 |
| --- | --- | --- | --- |
| `frontend` | `basic-platform/frontend:local` | 基础平台前端 + 合同管理前端 + 客户与商机管理前端 + 客户自助门户前端 | `127.0.0.1:8081` |
| `api` | `basic-platform:local` | 基础平台 API + Worker | 不发布 |
| `contract-api` | `contract-management/backend:local` | 合同管理 API + Temporal Worker | 不发布 |
| `customer-api` | `customer-opportunity/backend:local` | 客户与商机管理 API，只包含 `crm-server` | 不发布 |
| `customer-presale-alert-worker` | `customer-opportunity/backend:local` | TS-008 售前超时预警扫描和站内消息投影 | 不发布 |
| `portal-api` | `customer-portal/backend:local` | 客户自助门户 API，只包含 `portal-server`；仅在 `customer_portal/dev` 接入后启动 | 不发布 |

API 与 Worker 服务使用独立镜像目标、独立容器和对应运行配置，只通过 Compose 内网被统一 Nginx 或数据库/队列访问。CRM 与 Portal 虽然从同一个 `customer_and_opportunity` 源码仓库构建，但 Dockerfile 使用 `crm-runtime`、`portal-runtime`、`presale-alert-worker-runtime` 等目标：CRM 镜像不包含 `portal-server`，Portal 镜像不包含 `crm-server`。二者还使用独立 MySQL、独立 OIDC Client 和独立会话。

`docker-local.sh up` 会构建客户与商机 API 及预警 Worker 镜像，并在客户 profile 启动 `customer-presale-alert-worker`。客户门户在首次受控接入前不会创建常驻 `portal-api` 容器，因为此时 OIDC Client、租户、角色目录和最小权限机器凭据尚不存在；完成 `customer_portal/dev` 接入后，再次执行 `up` 或 `refresh-portal-api` 即会启动 Portal API。MySQL、Temporal、迁移任务和 provisioner 属于基础设施或一次性任务，不计为业务应用容器。

## 7. 远程服务器运行（测试/演示）

把 `docker-local.sh` 全家桶（含"一键接入"）跑在远程服务器上的完整步骤、备份、开机自启与
安全提醒见 [在远程服务器上运行本地统一编排](./server-local-orchestration.md)。

## 7. 网关并发锁

`portal-gateway.sh` 的读写命令都持有跨进程锁。Linux 使用 `flock`，无该命令时回退到原子目录锁。当前基础平台后端镜像已经安装 util-linux。

```dotenv
PORTAL_GATEWAY_LOCK_FILE=/path/to/portal-apps-locations.conf.lock
PORTAL_GATEWAY_LOCK_TIMEOUT=60
```

正常 Docker 启动不需要配置这些变量。

## 8. 本地测试数据

```bash
bash scripts/seed-local-test-data.sh --dry-run
bash scripts/seed-local-test-data.sh
```

默认目标是：

```text
compose.local.yaml
docker/.env.local
project = basic-platform-local
```

脚本只允许 `APP_ENV=development/test/local` 且 `MYSQL_DATABASE=basic_platform`，然后检查本地 mysql 服务和基础数据。不要用它操作生产数据库。

## 9. 常见问题

| 现象 | 处理 |
| --- | --- |
| `.env.local` 有占位符 | 补齐模板要求的 Secret，再执行 `docker-local.sh config` |
| API 更新后仍 404/405 | `bash scripts/docker-local.sh refresh-api` |
| Compose 配置错误 | `bash scripts/docker-local.sh config` |
| 依赖启动慢导致失败 | 查看对应服务日志；脚本已经分阶段等待并重试一次 |
| 网关锁超时 | 检查是否有并发 onboard/offboard/CI |
| LAN IP 改变 | 重新执行 `lan-access.sh enable`，或由 `docker-local.sh` 检测并更新覆盖 |
