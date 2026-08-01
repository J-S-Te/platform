# 生产环境 CI/CD 部署

> 更新日期：2026-08-01。生产目录承载 platform、frontend、contract 和 project 四个不可变镜像。

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
chmod 600 .env .release.env
```

替换所有 `REPLACE_WITH_...`。不要提交 `.env`、`.release.env`、私钥或备份。

## 2. 镜像仓库

- 四个仓库的 workflow 都使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`。
- 远端发布统一使用 `image@sha256:digest`，不使用可变 tag 作为最终发布标识。

## 3. GitHub Environment

四个仓库的 deploy job 当前使用 `test` Environment。配置：

### Secrets

- `DEPLOY_HOST`
- `DEPLOY_USER`
- `DEPLOY_PORT`（可选，默认 22）
- `DEPLOY_SSH_KEY`
- `DEPLOY_KNOWN_HOSTS`
- ACR 仓库额外需要 `ACR_USERNAME`、`ACR_PASSWORD`

### Variables

- `DEPLOY_PATH`（可选，默认 `/opt/basic-platform`）
- 每个仓库需要配置对应的 ACR variables

`DEPLOY_KNOWN_HOSTS` 必须在可信网络核对服务器指纹后生成。变量缺失时 deploy 任务会失败，不会跳过发布。

## 4. 首次上线

1. 发布 platform，使生产部署资产、平台镜像和迁移到位；
2. 初始化首个管理员；
3. 发布 frontend；
4. 在平台创建或核对 `contract_management/prod`、浏览器 Client、catalog-publisher Client 和精确回调；
5. 把受控接入结果写入服务器 `.env`；
6. 发布 contract。
7. 在平台创建或核对 `project_management/prod` 及其独立浏览器 Client；
8. 将 `PROJECT_OIDC_*` 和项目数据库密码写入服务器 `.env`，再发布 project。

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

不要把本地 `dev` Client、localhost 回调或局域网 HTTP 配置复制到生产。

## 5. 发布行为

CI 远端调用：

```bash
./bin/deploy-service.sh platform <image@sha256:digest>
./bin/deploy-service.sh frontend <image@sha256:digest>
./bin/deploy-service.sh contract <image@sha256:digest>
./bin/deploy-service.sh project <image@sha256:digest>
```

脚本使用 `flock` 串行发布，校验 Compose，后端发布前备份数据库，执行迁移，更新单个服务并检查健康状态。应用失败会恢复上一镜像；已成功执行的数据库迁移不会自动反向迁移。

发布不会删除或重建 Application、Environment、LoginTarget、OAuth Client，也不会覆盖服务器 `.env` 和 `.release.env`。

## 6. 恢复和备份

必须备份：

- platform MySQL；
- contract MySQL；
- project MySQL；
- 平台上传文件；
- JWT 密钥；
- `.env` 和 `.release.env` 的安全副本；
- 生产 Nginx 配置。

恢复演练要验证数据库、文件、Issuer、Client 凭据和上一镜像能共同恢复；只回退镜像不能逆转不兼容迁移。
