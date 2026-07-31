# 生产环境 CI/CD 部署

> 更新日期：2026-07-31
> 子系统开发、OIDC、安全回调和首次接入的完整要求见
> [`docs/subsystem-onboarding.md`](../../docs/subsystem-onboarding.md)。

该目录是三仓库共用的远程运行目录。三个项目各构建一个镜像：统一前端镜像同时
承载基础平台与合同管理前端；`platform-api` 在一个容器内运行基础平台 API 与 Worker；
`contract-api` 在一个容器内运行合同 API 与 Temporal Worker。迁移任务复用对应后端
镜像并在执行完成后退出，不会额外构建业务镜像。

## 1. 服务器初始化

服务器需要 Linux、Docker Engine、Docker Compose v2、Nginx、`curl`、`gzip`
和 `flock`。建议只开放 SSH、80、443，应用端口只监听 `127.0.0.1`。

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

## 2. GitHub 配置

在 `frontend`、`platform`、`contract_management` 三个 GitHub 仓库中创建
`production` Environment，并配置相同的 Actions secrets：

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

推送 `main` 后，流水线执行测试并构建、推送 ACR 镜像；不会因为尚未配置
生产服务器密钥而自动发起 SSH 部署。需要发布时，在 GitHub Actions 中从 `main`
手动运行 `platform-ci-cd`，并勾选 `deploy_production`。此时流水线才会通过 SSH
发布不可变的 `image@sha256:digest`；如果上述生产 Environment secrets 未配置，任务会
明确失败并指出缺失的配置。相同仓库及服务器上的发布会串行执行，避免中途取消或三个
仓库同时更新 Compose 状态。

## 3. 首次上线顺序

先手动触发或推送 `platform`，再部署 `frontend`。平台数据库迁移成功后初始化
管理员：

```bash
cd /opt/basic-platform
read -rsp "管理员密码: " ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" | docker compose \
  --env-file .env --env-file .release.env --profile release \
  run -T --rm platform-migrate ./bootstrap-admin \
  --display-name "平台管理员" --account-name admin --password-stdin
unset ADMIN_PASSWORD
```

### 3.1 当前生产 Compose 的限制

“统一登录目标”不在前端配置。当前本目录的生产 `compose.yaml` **没有**
`subsystem-provisioner` 服务，也没有把 Docker Socket、子系统项目目录和动态网关 include
授权给平台 API。因此仅部署本目录后，不能直接执行 `subsystem.sh onboard`（或兼容壳 `subsystem-onboarding.sh`）并假设它会：

- 写入子系统 `.env.local`；
- 启动子系统 Compose；
- 修改生产 Nginx；
- 自动发布授权目录。

生产环境必须先选择并评审一种方案：

1. 部署与本地相同安全边界的独立 provisioner，限制 Unix Socket 动作、项目目录、Docker
   网络、网关脚本和 include 路径；或
2. 由运维在受控变更单中完成运行配置、容器和 Nginx 部署，再通过平台管理 API登记相同
   Application/Environment/LoginTarget/OAuth Client。

禁止为了图方便给普通 `platform-api` 容器直接挂载 Docker Socket。

### 3.2 provisioner 已部署时的首次接入

平台 API 应通过本机回环或 HTTPS 调用。生产 OAuth 回调必须使用 HTTPS：

```bash
bash scripts/subsystem.sh onboard \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --environment prod \
  --public-base-url https://你的域名 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management \
  --client-type confidential \
  --api-base-url http://127.0.0.1:${PLATFORM_API_PORT:-18080}/api/v1 \
  --platform-origin https://你的域名 \
  --account admin
```

应用编码与公开路径统一使用 `contract_management`；公开路径固定为 `/contract_management`。
脚本调用后端原子创建应用、环境、相对登录目标和 OAuth Client，Client Secret 只在后端内存中
交给 provisioner，不返回浏览器或命令行。独立审计机器客户端仍按审计客户端管理流程创建，
不能复用浏览器 OIDC Client。完成运行时配置后再触发 `contract_management` 发布。

接入前先执行同一命令并增加 `--dry-run --yes`。接入只用于首次创建不存在的环境；常规
镜像发布、迁移、容器重启、回滚均不得执行 onboard/offboard。

### 3.3 生产安全要求

- `.env`、`.release.env` 和子系统 `.env.local` 权限保持 `0600`；
- `AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=false`；
- 不启用 `BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API`；管理 API 使用回环或 HTTPS；
- 浏览器 Client、授权目录 publisher、审计客户端使用独立凭据；
- 网关更新使用 `portal-gateway.sh` 的跨进程锁。Linux 应安装 util-linux `flock`；
- offboard 不是跨 Docker/文件/DB 的全局事务，执行前必须备份子系统运行配置并准备补偿；
- Secret 不放入命令行、GitHub Actions 普通变量或命令输出。

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
