# 本地 Docker 与脚本使用说明

> 更新日期：2026-07-31

## 1. 推荐入口

在 `platform` 目录执行：

```bash
bash scripts/docker-local.sh up
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh verify
bash scripts/docker-local.sh logs api subsystem-provisioner
```

`docker-local.sh up`：

- 只在缺失时从模板创建 `platform/docker/.env.local` 和子系统 `.env.local`；
- 不要求重建已有 `.env.local`；
- 构建基础平台、合同后端和统一前端镜像；
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
| `../<subsystem>/.env.example` | 子系统模板 | 否 |
| `../<subsystem>/.env.local` | 子系统实际配置 | onboard 时受控更新 |
| `docker/.env.lan` | 临时局域网覆盖 | `lan-access.sh` 管理 |

所有实际 Secret 文件必须保持 `0600`，禁止提交。

`prepare-docker-env.sh` 是旧 `compose.yaml + docker/.env` 流程的辅助工具，不是当前 `compose.local.yaml` 的推荐入口。它不会覆盖已有 `docker/.env`，因为仅重写文件密码会与已有 MySQL volume 中的账号密码失配。

## 4. 局域网访问

```bash
bash scripts/lan-access.sh enable --address 192.168.3.11 --port 8081
bash scripts/lan-access.sh status
bash scripts/lan-access.sh disable
```

enable 会生成权限为 `0600` 的临时覆盖文件并重建 API、合同 API 和 frontend。它保留数据库和统一登录控制面记录。该模式使用 HTTP，只适用于可信局域网。

局域网 OAuth HTTP 回调与接入脚本访问平台 API 是两套策略，详见 [子系统开发与统一身份接入手册](./subsystem-onboarding.md#33-http-回调策略)。

## 5. 定向更新

```bash
bash scripts/docker-local.sh refresh-api
bash scripts/docker-local.sh refresh-frontend
bash scripts/docker-local.sh refresh-contract-api
```

定向更新不会重新接入子系统：

- `refresh-api`：迁移并重建基础平台 API/provisioner；
- `refresh-frontend`：仅重建统一前端；
- `refresh-contract-api`：合同迁移并重建合同后端。

## 6. 网关并发锁

`portal-gateway.sh` 的读写命令都持有跨进程锁。Linux 使用 `flock`，无该命令时回退到原子目录锁。当前基础平台后端镜像已经安装 util-linux。

```dotenv
PORTAL_GATEWAY_LOCK_FILE=/path/to/portal-apps-locations.conf.lock
PORTAL_GATEWAY_LOCK_TIMEOUT=60
```

正常 Docker 启动不需要配置这些变量。

## 7. 本地测试数据

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

## 8. 常见问题

| 现象 | 处理 |
| --- | --- |
| `.env.local` 有占位符 | 补齐模板要求的 Secret，再执行 `docker-local.sh config` |
| API 更新后仍 404/405 | `bash scripts/docker-local.sh refresh-api` |
| Compose 配置错误 | `bash scripts/docker-local.sh config` |
| 依赖启动慢导致失败 | 查看对应服务日志；脚本已经分阶段等待并重试一次 |
| 网关锁超时 | 检查是否有并发 onboard/offboard/CI |
| LAN IP 改变 | 重新执行 `lan-access.sh enable`，或由 `docker-local.sh` 检测并更新覆盖 |

