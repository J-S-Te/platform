# 生产环境 CI/CD 部署

> 更新日期：2026-08-03。生产目录承载 platform、frontend 和 contract 三个不可变镜像。

## 1. 服务器要求

- Linux、Docker Engine、Docker Compose v2、`curl`、`gzip`、`flock`；
- 低权限发布用户可访问 Docker，并拥有部署目录；
- 推荐 Nginx/负载均衡终止 HTTPS，只开放 SSH、80、443；
- 部署目录默认 `/opt/basic-platform`。

```bash
sudo install -d -o deploy -g deploy -m 750 /opt/basic-platform
cd /opt/basic-platform
cp .env.example .env
cp .release.env.example .release.env
install -d -m 700 runtime
cp contract.env.example runtime/contract.env
chmod 600 .env .release.env runtime/contract.env
```

替换 `.env` 中所有基础设施占位值；`.release.env` 的镜像 digest 由 CI/CD 发布自动更新。`runtime/contract.env` 内的 OIDC、授权目录和审计占位值由基础平台接入页面和生产 Agent 自动替换。不要提交运行环境文件、私钥或备份。

## 2. 镜像仓库

- `platform` 和 `frontend` workflow 使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`。
- `contract_management` workflow 当前推送 GHCR，并使用仓库 `GITHUB_TOKEN`；服务器必须能够拉取对应 GHCR 包。
- 远端发布统一使用 `image@sha256:digest`，不使用可变 tag 作为最终发布标识。

## 3. GitHub Environment

三个仓库的 deploy job 当前使用 `test` Environment。配置：

### Secrets

- `DEPLOY_HOST`
- `DEPLOY_USER`
- `DEPLOY_PORT`（可选，默认 22）
- `DEPLOY_SSH_KEY`
- `DEPLOY_KNOWN_HOSTS`
- ACR 仓库额外需要 `ACR_USERNAME`、`ACR_PASSWORD`

### Variables

- `DEPLOY_PATH`（可选，默认 `/opt/basic-platform`）
- platform/frontend 仓库需要对应 ACR variables

`DEPLOY_KNOWN_HOSTS` 必须在可信网络核对服务器指纹后生成。变量缺失时 deploy 任务会失败，不会跳过发布。

## 4. 首次上线

1. 发布 platform，使生产部署资产、平台镜像和迁移到位；
2. 初始化首个管理员；
3. 发布 frontend，并确认 `platform-api` 与隔离的 `subsystem-provisioner` 健康；
4. 发布 contract 不可变镜像。若尚未接入，发布脚本只安全暂存 digest，不会因为 OIDC 占位值而启动失败；
5. 登录基础平台，在“应用接入”中选择合同管理系统和 `prod` 环境完成接入；
6. 平台自动创建 `contract_management/prod`、浏览器 Client、catalog-publisher Client、审计 Client、精确回调和初始管理员授权，同时将一次性凭据安全写入服务器 `runtime/contract.env`，执行合同备份、迁移并重建 `contract-api`。

生产接入不再要求管理员在命令行复制 OAuth Client Secret。Secret 只在平台后端内存、受限 Unix Socket 和权限为 `0600` 的服务器 `runtime/contract.env` 之间流转，不返回浏览器，也不进入命令行参数或日志。生产 Agent 只允许 `contract_management/prod` 和固定 Compose 服务，不接受浏览器指定的文件、命令、镜像或服务名。

首次接入前，`runtime/contract.env` 中的以下字段可以保留占位值，由平台接入流程替换：

```text
OIDC_CLIENT_ID
OIDC_CLIENT_SECRET
OIDC_TENANT_ID
PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET
PLATFORM_AUDIT_CLIENT_ID
PLATFORM_AUDIT_CLIENT_SECRET
```

`SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT` 必须填写当前生产部署目录的规范绝对路径，默认 `/opt/basic-platform`。`SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID` 默认对应迁移内置租户，标准单租户部署不需要额外配置。平台镜像更新后会同时重建 `platform-api` 和 `subsystem-provisioner`，二者通过共享 Unix Socket 通信；只有 Agent 挂载 Docker Socket。

完成上述一次性服务器初始化后，Application、Environment、登录目标、OAuth Client、运行时凭据、首次管理员授权、失败重试和安全下线都从基础平台页面操作。日常管理员不需要登录服务器或执行子系统接入脚本；服务器命令仅保留给 CI/CD、首个管理员初始化和基础设施故障恢复。

管理员初始化：

```bash
cd /opt/basic-platform
read -rsp "管理员密码: " ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" | docker compose \
  --env-file .env --env-file .release.env --profile release \
  run -T --rm platform-migrate ./bootstrap-admin \
  --display-name "平台管理员" --account-name admin --password-stdin
unset ADMIN_PASSWORD
```

生产合同回调应使用：

```text
https://<正式域名>/contract_management/auth/callback
```

不要把本地 `dev` Client、localhost 回调或局域网 HTTP 配置复制到生产，也不要手工修改平台数据库伪造 `READY` 状态。

## 5. 发布行为

CI 远端调用：

```bash
./bin/deploy-service.sh platform <image@sha256:digest>
./bin/deploy-service.sh frontend <image@sha256:digest>
./bin/deploy-service.sh contract <image@sha256:digest>
```

脚本使用 `flock` 串行发布，校验 Compose，后端发布前备份数据库，执行迁移，更新单个服务并检查健康状态。应用失败会恢复上一镜像；已成功执行的数据库迁移不会自动反向迁移。首次发布 contract 且接入凭据仍为占位值时只保存不可变镜像指针，实际迁移和启动由基础平台页面接入触发。

CI 发布不会删除或重建 Application、Environment、LoginTarget、OAuth Client，也不会覆盖服务器 `.env`、`.release.env` 或 `runtime/contract.env`；仅基础平台首次接入会原子更新 `runtime/contract.env` 的固定白名单字段。

## 6. 恢复和备份

必须备份：

- platform MySQL；
- contract MySQL；
- 平台上传文件；
- JWT 密钥；
- `.env`、`.release.env` 和 `runtime/contract.env` 的安全副本；
- 生产 Nginx 配置。

恢复演练要验证数据库、文件、Issuer、Client 凭据和上一镜像能共同恢复；只回退镜像不能逆转不兼容迁移。
