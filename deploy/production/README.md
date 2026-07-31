# 生产环境 CI/CD 部署

> 更新日期：2026-07-31
> 子系统开发、OIDC、安全回调和首次接入的完整要求见
> [`docs/subsystem-onboarding.md`](../../docs/subsystem-onboarding.md)。

该目录是三仓库共用的远程运行目录。三个项目各构建一个镜像：统一前端镜像同时
承载基础平台与合同管理前端；`platform-api` 在一个容器内运行基础平台 API 与 Worker；
`contract-api` 在一个容器内运行合同 API 与 Temporal Worker。迁移任务复用对应后端
镜像并在执行完成后退出，不会额外构建业务镜像。

## 1. 服务器初始化

服务器需要 Linux、Docker Engine、Docker Compose v2、`curl`、`gzip`
和 `flock`。正式域名和 HTTPS 部署建议使用主机 Nginx，只开放 SSH、80、443，
应用端口监听 `127.0.0.1`。

```bash
sudo install -d -o deploy -g deploy -m 750 /opt/basic-platform
```

先把 `platform/deploy/production/` 的内容复制到该目录，再以发布用户登录服务器：

```bash
cd /opt/basic-platform
cp .env.example .env
cp .release.env.example .release.env
chmod 600 .env .release.env
docker login <ACR 专有网络域名>
```

把示例域名和所有 `REPLACE_WITH_...` 替换为真实值。密钥可用以下方式生成：

```bash
openssl rand -base64 32
openssl rand -hex 32
```

ACR 仓库为私有时，服务器必须使用有 Pull 权限的 ACR 访问凭证登录。ECS 与
ACR 在同一 VPC 时应使用控制台提供的专有网络域名。
不要提交 `.env`、`.release.env` 或备份文件。

把 `nginx/basic-platform.conf.example` 复制到主机 Nginx 配置目录，替换域名和
证书路径后执行 `sudo nginx -t` 并重载 Nginx。

没有域名和主机 Nginx 的测试服务器，可以让统一前端直接承载公网 HTTP 入口：

```dotenv
FRONTEND_BIND_ADDRESS=0.0.0.0
FRONTEND_PORT=8081
APP_PUBLIC_BASE_URL=http://47.111.20.119:8081
APP_CORS_ALLOWED_ORIGINS=http://47.111.20.119:8081
OIDC_ISSUER=http://47.111.20.119:8081
AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=true
AUTH_SESSION_COOKIE_SECURE=false
OIDC_SESSION_COOKIE_SECURE=false
```

此模式不提供 TLS，只适用于受控测试环境；安全组需要放行 TCP/8081。

## 2. GitHub 配置

当前三个 workflow 使用 `test` Environment。在 `frontend`、`platform`、
`contract_management` 三个 GitHub 仓库中创建该 Environment，并配置相同的
Actions secrets：

- `ACR_USERNAME`：有镜像 Push 权限的 ACR 登录名
- `ACR_PASSWORD`：对应的 ACR 访问凭证密码
- `DEPLOY_HOST`：服务器地址
- `DEPLOY_USER`：拥有该目录且可运行 Docker 的低权限发布用户
- `DEPLOY_PORT`：SSH 端口，可省略，默认 22
- `DEPLOY_SSH_KEY`：发布用户的 SSH 私钥
- `DEPLOY_KNOWN_HOSTS`：用 `ssh-keyscan -H <host>` 预先核验后保存的主机公钥

同时配置以下 Actions variables：

- `ACR_PUSH_REGISTRY`：供 GitHub 托管 Runner 使用的 ACR 公网域名
- `ACR_PULL_REGISTRY`：供 ECS 使用的同一 ACR 实例专有网络域名
- `ACR_NAMESPACE`：ACR 命名空间
- `ACR_REPOSITORY`：当前仓库对应的 ACR 镜像仓库名称
- `DEPLOY_PATH`：可选，默认为 `/opt/basic-platform`

平台仓库每次发布还会
同步本目录的 Compose 和发布脚本；不会覆盖服务器上的 `.env` 与
`.release.env`。

推送 `main` 后，流水线会执行测试、构建并推送 ACR 镜像，然后自动通过 SSH
发布不可变的 `image@sha256:digest`。Environment secrets 缺失时部署任务会明确失败。
三个仓库可能同时到达服务器，但远端 `flock` 会把 Compose 更新串行化。

## 3. 首次上线顺序

首次上线依次推送 `platform`、`frontend`。平台数据库迁移成功后初始化管理员：

```bash
cd /opt/basic-platform
read -rsp "管理员密码: " ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" | docker compose \
  --env-file .env --env-file .release.env --profile release \
  run -T --rm platform-migrate ./bootstrap-admin \
  --display-name "平台管理员" --account-name admin --password-stdin
unset ADMIN_PASSWORD
```

随后在平台应用管理中为已有的 `contract_management` 应用完成生产环境配置，并创建两套
独立 OAuth Client：

- 浏览器 Client：`authorization_code + refresh_token`、PKCE、`openid profile`，
  回调地址为 `http://47.111.20.119:8081/contract_management/auth/callback`。
- 目录发布 Client：`client_credentials + client_secret_basic`，只授予
  `authorization.catalog.sync`。

把应用 ID、两套 Client 凭据、Tenant ID 和精确回调地址写入服务器 `.env`。
`PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED` 必须设为 `true`。审计机器 Client
仍需单独创建，不能复用上述 Client。配置完成后再推送 `contract_management`。

## 4. 发布与恢复

CI 实际调用：

```bash
./bin/deploy-service.sh platform <acr-vpc-host>/<namespace>/platform@sha256:<digest>
./bin/deploy-service.sh frontend <acr-vpc-host>/<namespace>/frontend@sha256:<digest>
./bin/deploy-service.sh contract <acr-vpc-host>/<namespace>/contract_management@sha256:<digest>
```

脚本串行锁定发布，拉取镜像、校验 Compose、在后端发布前备份对应 MySQL、
执行迁移、更新单个后端容器并检查健康状态。失败时恢复上一镜像；已经成功执行的数据库
迁移不会自动反向迁移。数据库备份和历史 release 文件位于 `backups/`。
平台上传文件、JWT 密钥和 Docker volumes 仍应纳入服务器级定期异地备份。

发布脚本和子系统接入互相独立：发布新镜像不会删除或重建 Application、Environment、
LoginTarget、OAuth Client；已接入子系统也不需要重新执行接入脚本。
