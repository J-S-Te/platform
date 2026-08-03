# 生产环境 CI/CD 部署

> 更新日期：2026-08-03。生产目录承载 platform、frontend、contract、CRM 和客户 Portal 不可变镜像。

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
cp customer.env.example runtime/customer.env
cp portal.env.example runtime/portal.env
chmod 600 .env .release.env runtime/contract.env runtime/customer.env runtime/portal.env
```

替换 `.env` 中所有基础设施占位值；`.release.env` 的镜像 digest 由 CI/CD 发布自动更新。`runtime/contract.env`、`runtime/customer.env`、`runtime/portal.env` 中的 OIDC、授权目录和服务 Client 占位值由基础平台接入页面和生产 Agent 按审核清单替换。CRM/Portal 模板中的业务加密密钥仍必须由部署人员先生成并替换；Agent 不生成或覆盖这些长期业务密钥。首次镜像发布在接入凭据未补齐时只安全暂存 digest，不启动数据库迁移或 API。不要提交运行环境文件、私钥或备份。

本节是**一次性基础设施初始化**，由部署人员或 CI/CD 完成，不是每次接入子系统都要执行的管理员命令。Docker/Compose、镜像仓库访问、平台密钥、数据库、部署目录和隔离 Agent 准备完成后，日常平台管理员只使用基础平台“应用接入”页面。

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
- customer_and_opportunity 仓库可设置 `CUSTOMER_DEPLOY_SCRIPT`（默认 `/opt/basic-platform/bin/deploy-customer-opportunity.sh`）

`DEPLOY_KNOWN_HOSTS` 必须在可信网络核对服务器指纹后生成。变量缺失时 deploy 任务会失败，不会跳过发布。

## 4. 首次上线

1. 发布 platform，使生产部署资产、平台镜像和迁移到位；
2. 初始化首个管理员；
3. 发布 frontend，并确认 `platform-api` 与隔离的 `subsystem-provisioner` 健康；
4. 发布所需子系统的不可变镜像。尚未接入时，发布脚本只安全暂存 digest，不会因 OIDC 占位值启动失败；
5. 登录基础平台，在“应用接入”中从服务器审核目标列表选择 `contract_management/prod`、`customer_and_opportunity/prod` 或 `customer_portal/prod`；客户 Portal 依赖 CRM，必须先完成 `customer_and_opportunity/prod`；
6. 平台自动创建应用环境、浏览器 Client、catalog-publisher Client、按用途拆分的服务 Client、精确回调和适用的初始管理员授权；Agent 将一次性凭据写入对应的 `runtime/*.env`，再按目标执行固定备份、迁移和 API 重建。

生产接入不再要求管理员在命令行复制 OAuth Client Secret。Secret 只在平台后端内存、受限 Unix Socket 和权限为 `0600` 的服务器 `runtime/*.env` 之间流转，不返回浏览器，也不进入命令行参数或日志。生产 Agent 只允许 `subsystems.d/*.yaml` 中随发布包审核的应用/环境和固定 Compose 服务，不接受浏览器指定的文件、命令、镜像或服务名。

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

`SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT` 必须填写当前生产部署目录的规范绝对路径，默认 `/opt/basic-platform`。`SUBSYSTEM_PRODUCTION_PROFILES_DIR` 默认 `/opt/basic-platform/subsystems.d`，必须位于部署根内、不能是符号链接，目录和清单不能组/全局可写。`SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID` 默认对应迁移内置租户，标准单租户部署不需要额外配置。平台镜像更新后会同时重建 `platform-api` 和 `subsystem-provisioner`，二者分别只读同一清单并通过共享 Unix Socket 通信；只有 Agent 挂载 Docker Socket。

### 4.1 新增生产子系统目标（部署人员）

新增子系统时不再向 `.env` 增加一组 `SUBSYSTEM_PRODUCTION_APPLICATION_*` 白名单。部署人员应在代码评审中新增 `subsystems.d/<application>-<environment>.yaml`，并同步准备 Compose 服务、不可变镜像键和 `runtime/*.env.example`。清单只允许声明：

- 应用编码、环境、固定 PathPrefix/UpstreamURL 和客户端类型；
- 部署根 `runtime/` 下的环境文件，以及平台输入到明确环境变量的绑定；
- 固定 Compose profile、依赖、数据库备份目标、迁移服务、运行服务和下线服务；
- `.release.env` 中必须为 `image@sha256:digest` 的镜像键。

清单不能声明 shell 命令、脚本、任意宿主机绝对路径，也不能选择 `platform-api`、`platform-mysql`、`frontend`、`subsystem-provisioner`、`PLATFORM_IMAGE` 或 `FRONTEND_IMAGE`。未知 YAML 字段、重复应用/环境、未知凭据来源、路径逃逸、符号链接和不安全权限都会使平台或 Agent 启动失败。当前随包审核目标见 [`subsystems.d/`](./subsystems.d/)；发布清单后管理页面会自动出现新选项，无需逐台服务器手工维护应用白名单。

最小示例：

```yaml
version: 1
default: false
application:
  code: billing
  name: 计费系统
  description: 计费与对账
  environment: prod
  path_prefix: /billing
  upstream_url: http://billing-api:8080
  client_type: confidential
runtime:
  required_infrastructure_keys: [BILLING_MYSQL_PASSWORD, BILLING_MYSQL_ROOT_PASSWORD]
  files:
    - path: runtime/billing.env
      compose_environment_key: BILLING_RUNTIME_ENV_FILE
      required_existing_keys: [BILLING_ENCRYPTION_KEY]
      values:
        OIDC_SCOPES: openid profile
      bindings:
        OIDC_ISSUER: issuer
        OIDC_CLIENT_ID: client_id
        OIDC_CLIENT_SECRET: client_secret
        OIDC_REDIRECT_URI: redirect_uri
        OIDC_TENANT_ID: tenant_id
        OIDC_SESSION_COOKIE_SECURE: cookie_secure
        PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID: application_id
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID: catalog_publisher_client_id
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET: catalog_publisher_client_secret
        PLATFORM_AUDIT_CLIENT_ID: service.audit_ingest.client_id
        PLATFORM_AUDIT_CLIENT_SECRET: service.audit_ingest.client_secret
compose:
  profiles: [billing, billing-release]
  dependency_services: [billing-mysql]
  database: {service: billing-mysql, name: billing}
  migrate_service: billing-migrate
  runtime_services: [billing-api]
  teardown_services: [billing-api]
  release_image_keys: [BILLING_IMAGE]
```

`bindings` 的值不是模板表达式，而是 Agent 内置的有限数据源。通用来源包括 `issuer`、`client_id`、`client_secret`、`redirect_uri`、`public_url`、`tenant_id`、`application_id`、`application_code`、`environment`、`path_prefix`、`cookie_secure` 和 catalog-publisher 凭据；用途 Client 使用 `service.<purpose>.client_id` / `service.<purpose>.client_secret`。如果子系统需要新的机器用途，应先在平台控制面增加最小 scope 的用途 Client，再在清单引用，不能复用浏览器 Client。

完成上述一次性服务器初始化后，Application、Environment、登录目标、OAuth Client、运行时凭据、首次管理员授权、失败重试和安全下线都从基础平台页面操作。接入时未另选初始管理员则使用当前操作者；平台会保存这一选择，首次部署失败后的页面重试仍使用原选择，不会改授给点击重试的人。初始授权完成后，普通更新或重试不会恢复后来主动移除的角色。

日常管理员不需要登录服务器、执行子系统接入脚本或在命令行复制 OAuth 配置；服务器命令仅保留给 CI/CD、首个管理员初始化和基础设施故障恢复。部署失败时先读取环境卡片的脱敏错误与“下一步操作”，修复后点击“重试”，不要重复新增接入。

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
./bin/deploy-customer-opportunity.sh <crm-image@sha256:digest> <portal-image@sha256:digest>
```

脚本使用 `flock` 串行发布，校验 Compose，后端发布前备份数据库，执行迁移，更新单个服务并检查健康状态。应用失败会恢复上一镜像；已成功执行的数据库迁移不会自动反向迁移。首次发布 contract 且接入凭据仍为占位值时只保存不可变镜像指针，实际迁移和启动由基础平台页面接入触发。

CI 发布不会删除或重建 Application、Environment、LoginTarget、OAuth Client，也不会覆盖服务器 `.env`、`.release.env` 或 `runtime/*.env`；它会更新随代码审核的 `subsystems.d` 清单。仅基础平台首次接入会更新目标清单明确列出的运行时字段，其他长期业务密钥保持不变。

客户与商机发布使用独立 `customer` Compose profile，不影响既有 platform/frontend/contract 发布。脚本先校验两个 ACR 不可变 digest，然后更新并备份 `.release.env`。如果 `.env`、`runtime/customer.env` 或 `runtime/portal.env` 仍含 `REPLACE_WITH_*`/`PENDING_*`，脚本只暂存镜像，不启动服务。配置完整后依次启动双库、生成一致性备份、执行两个 schema 的语句级迁移，再切换 CRM 和 Portal API 并检查健康状态。

语句级迁移表为 `app_schema_migration_statements`。每条 SQL 在执行前记录 `RUNNING`，成功后才改为 `APPLIED`；重启发现遗留 `RUNNING` 会拒绝继续。必须人工核验对应表、列、索引、约束和必要回填后，按变更评审结果处理该检查点，禁止把 duplicate table/column/index 笼统视为成功。迁移器只自动接管空 schema；已有业务表但没有生产元数据表时拒绝发布，必须先建立经评审的生产基线。

### 安装或升级客户商机生产资产

将本目录的 `compose.yaml`、`customer.env.example`、`portal.env.example`、`bin/deploy-customer-opportunity.sh` 和 Nginx 示例同步到服务器同名路径，然后执行：

```bash
cd /opt/basic-platform
install -d -m 700 runtime backups backups/releases
test -f runtime/customer.env || install -m 600 customer.env.example runtime/customer.env
test -f runtime/portal.env || install -m 600 portal.env.example runtime/portal.env
chmod 600 .env .release.env runtime/customer.env runtime/portal.env
chmod 750 bin/deploy-customer-opportunity.sh
docker compose --env-file .env --env-file .release.env config --quiet
```

已有运行配置禁止用模板覆盖，只补新增字段。更新 Nginx 后先执行 `nginx -t`，再 reload。GitHub `test` Environment 的 `CUSTOMER_DEPLOY_SCRIPT` 必须与实际安装绝对路径一致。

## 6. 恢复和备份

必须备份：

- platform MySQL；
- contract MySQL；
- 平台上传文件；
- JWT 密钥；
- `.env`、`.release.env` 和 `runtime/contract.env` 的安全副本；
- `runtime/customer.env` 与 `runtime/portal.env` 的安全副本；
- 生产 Nginx 配置。

恢复演练要验证数据库、文件、Issuer、Client 凭据和上一镜像能共同恢复；只回退镜像不能逆转不兼容迁移。
