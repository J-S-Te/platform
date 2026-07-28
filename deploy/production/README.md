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

推送 `main` 后，流水线依次执行测试、构建并推送 GHCR 镜像、通过 SSH 发布
不可变的 `image@sha256:digest`。同一仓库及服务器上的发布会串行执行，避免中途
取消或三个仓库同时更新 Compose 状态。

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

登录平台后按“平台接入说明”创建 `contract-management` 子系统和独立审计机器
客户端。推荐值：

- 应用编码：`contract-management`
- 接入类型：Confidential OIDC
- 外部路径：`/contract`
- 内部上游：`http://contract-api:8081`
- 回调：`https://你的域名/contract/auth/callback`
- 登出回调：`https://你的域名/contract/logged-out`

把平台返回的租户 ID、OIDC Client ID/Secret 和审计 Client ID/Secret 写入
服务器 `.env`，然后触发 `contract_management` 发布。

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
