# 生产环境 CI/CD 部署

该目录是三仓库共用的远程运行目录。三个项目各构建一个镜像；`platform`
和 `contract_management` 的 API、Worker、迁移任务分别复用各自同一个镜像，
不会额外构建镜像。

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
docker login ghcr.io
```

把示例域名和所有 `REPLACE_WITH_...` 替换为真实值。密钥可用以下方式生成：

```bash
openssl rand -base64 32
openssl rand -hex 32
```

GHCR 包为私有时，服务器登录使用的 PAT 至少需要 `read:packages`。
不要提交 `.env`、`.release.env` 或备份文件。

把 `nginx/basic-platform.conf.example` 复制到主机 Nginx 配置目录，替换域名和
证书路径后执行 `sudo nginx -t` 并重载 Nginx。

## 2. GitHub 配置

在 `frontend`、`platform`、`contract_management` 三个 GitHub 仓库中创建
`production` Environment，并配置相同的 Actions secrets：

- `DEPLOY_HOST`：服务器地址
- `DEPLOY_USER`：拥有该目录且可运行 Docker 的低权限发布用户
- `DEPLOY_PORT`：SSH 端口，可省略，默认 22
- `DEPLOY_SSH_KEY`：发布用户的 SSH 私钥
- `DEPLOY_KNOWN_HOSTS`：用 `ssh-keyscan -H <host>` 预先核验后保存的主机公钥

可选仓库变量 `DEPLOY_PATH` 默认为 `/opt/basic-platform`。平台仓库每次发布还会
同步本目录的 Compose 和发布脚本；不会覆盖服务器上的 `.env` 与
`.release.env`。

推送 `main` 后，流水线只执行测试并构建、推送 GHCR 镜像；不会因为尚未配置
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

“统一登录目标”不在前端配置。生产接入前必须先部署并启用受控 subsystem provisioner；
当前生产 Compose 若未包含该服务，不能直接执行接入脚本并假设它能修改服务器文件或 Docker。
完成 provisioner 部署后，在平台目录执行：

```bash
bash scripts/subsystem-onboarding.sh \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --environment prod \
  --public-base-url https://你的域名 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management \
  --client-type confidential \
  --account admin
```

应用编码与公开路径统一使用 `contract_management`；公开路径固定为 `/contract_management`。
脚本调用后端原子创建应用、环境、相对登录目标和 OAuth Client，Client Secret 只在后端内存中
交给 provisioner，不返回浏览器或命令行。独立审计机器客户端仍按审计客户端管理流程创建，
不能复用浏览器 OIDC Client。完成运行时配置后再触发 `contract_management` 发布。

## 4. 发布与恢复

CI 实际调用：

```bash
./bin/deploy-service.sh platform ghcr.io/owner/platform@sha256:<digest>
./bin/deploy-service.sh frontend ghcr.io/owner/frontend@sha256:<digest>
./bin/deploy-service.sh contract ghcr.io/owner/contract_management@sha256:<digest>
```

脚本串行锁定发布，拉取镜像、校验 Compose、在后端发布前备份对应 MySQL、
执行迁移、更新服务并检查健康状态。失败时恢复上一镜像；已经成功执行的数据库
迁移不会自动反向迁移。数据库备份和历史 release 文件位于 `backups/`。
平台上传文件、JWT 密钥和 Docker volumes 仍应纳入服务器级定期异地备份。
