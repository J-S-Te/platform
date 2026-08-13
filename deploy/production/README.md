# 生产环境 CI/CD 部署

> 更新日期：2026-08-04。生产目录承载 platform、frontend、contract、CRM、客户 Portal 和项目管理系统不可变镜像。

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
chmod 600 .env .release.env
```

替换 `.env` 中所有基础设施占位值；`.release.env` 的镜像 digest 由 CI/CD 发布自动更新。`runtime/*.env` 不要求管理员手工创建：首次接入时，Agent 会从审核清单指定的 `*.env.example` 初始化缺失文件、自动收紧为 `0600`，写入 OIDC/授权目录/用途 Client，并为清单声明的业务密钥生成一次性 32 字节随机 base64 值。已有合法密钥、未知环境变量、注释及清单外文件都会保留，重试或更新不会轮换。部署人员也可以提前通过 Secret 管理系统写入合法密钥，Agent 会继续复用。首次镜像发布在接入凭据未补齐时只安全暂存 digest，不启动数据库迁移或 API。不要提交运行环境文件、私钥或备份。

Keycloak 的独立数据库、可选 bootstrap service account、HTTP/HTTPS 网关策略、轮换、备份恢复、HA、监控告警和灾备演练遵循 [Keycloak 生产运维 Runbook](../../docs/keycloak-production-operations.md)。该 Runbook 不强制 HTTPS：是否使用 TLS 由入口网关和 `KEYCLOAK_PUBLIC_URL` 决定。

本节是**一次性基础设施初始化**，由部署人员或 CI/CD 完成，不是每次接入子系统都要执行的管理员命令。Docker/Compose、镜像仓库访问、平台密钥、数据库、部署目录和隔离 Agent 准备完成后，日常平台管理员只使用基础平台“应用接入”页面。

应用接入不会在**预检阶段**要求所有平台级密钥都已经替换完成。预检只检查审核清单、模板、文件权限、镜像摘要和 Compose 配置；真正发布目标子系统时，Agent 只校验该目标声明的数据库凭据。当前四个生产目标分别需要 `CONTRACT_MYSQL_*`、`CUSTOMER_MYSQL_*`、`CUSTOMER_MYSQL_* + PORTAL_MYSQL_*` 或 `PROJECT_MYSQL_*`。数据库密码一旦用于持久化 MySQL，就不能由 Agent 随意生成或轮换，否则会导致已有数据库无法登录；因此它们仍需在第一次发布前由部署人员初始化。其他平台级密钥由平台自身启动和 CI/CD 初始化流程负责，不应阻塞无关子系统的预检。

测试服务器（一次性验证接入流程、没有历史数据）可在 `.env` 中设置 `SUBSYSTEM_PRODUCTION_ALLOW_PLACEHOLDER_DATABASE_CREDENTIALS=true`，Agent 将跳过数据库凭据校验，让 Compose 在首次创建空数据库时使用占位密码完成接入。该开关只应出现在测试服务器：已有持久化数据的数据库不会因为开关而自动改密，生产环境必须保持 `false`。

## 2. 镜像仓库

- `platform` 和 `frontend` workflow 使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`。
- `contract_management` workflow 当前推送 GHCR，并使用仓库 `GITHUB_TOKEN`；服务器必须能够拉取对应 GHCR 包。
- `project_management` workflow 与 `platform`/`frontend` 一致使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`；服务器必须能够拉取对应 ACR 包。
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
- platform/frontend 仓库需要对应 ACR variables
- customer_and_opportunity 仓库可设置 `CUSTOMER_DEPLOY_SCRIPT`（默认 `/opt/basic-platform/bin/deploy-customer-opportunity.sh`）

`DEPLOY_KNOWN_HOSTS` 必须在可信网络核对服务器指纹后生成。变量缺失时 deploy 任务会失败，不会跳过发布。

## 4. 首次上线

1. 发布 platform，使生产部署资产、平台镜像和迁移到位；
2. 初始化首个管理员；
3. 发布 frontend，并确认 `platform-api` 与隔离的 `subsystem-provisioner` 健康；
4. 发布所需子系统的不可变镜像。尚未接入时，发布脚本只安全暂存 digest，不会因 OIDC 占位值启动失败；
5. 登录基础平台，在“应用接入”中从服务器审核目标列表选择 `contract_management/prod`、`customer_and_opportunity/prod`、`customer_portal/prod` 或 `project_management/prod`；客户 Portal 依赖 CRM，必须先完成 `customer_and_opportunity/prod`；
6. 平台自动创建应用环境、浏览器 Client、catalog-publisher Client、按用途拆分的服务 Client、精确回调和适用的初始管理员授权；Agent 自动初始化对应 `runtime/*.env`、生成清单声明的长期业务密钥并写入一次性凭据，再按目标执行固定备份、迁移和 API 重建。

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

新增子系统时不再向 `.env` 增加一组 `SUBSYSTEM_PRODUCTION_APPLICATION_*` 白名单。部署人员应在代码评审中新增 `subsystems.d/<application>-<environment>.yaml`，并同步准备 Compose 服务、不可变镜像键和 `subsystem-templates/*.env.example`。该模板目录整体只读挂载给 Agent，后续新增模板不需要再修改 Compose volume。清单只允许声明：

- 应用编码、环境、固定 PathPrefix/UpstreamURL 和客户端类型；
- 部署根 `runtime/` 下的环境文件、受控初始化模板、可首次生成的 base64 密钥，以及平台输入到明确环境变量的绑定；
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
  # 仅声明该子系统实际使用且不可安全自动轮换的数据库凭据。
  # 这些键在真正 Provision 前校验，不会因为预检阶段仍有平台级占位值而阻止登记接入。
  required_infrastructure_keys: [BILLING_MYSQL_PASSWORD, BILLING_MYSQL_ROOT_PASSWORD]
  files:
    - path: runtime/billing.env
      template_path: subsystem-templates/billing.env.example
      compose_environment_key: BILLING_RUNTIME_ENV_FILE
      required_existing_keys: []
      generated_keys: [BILLING_ENCRYPTION_KEY_BASE64]
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

`template_path` 是部署根内随版本审核的 `*.env.example` 相对路径；缺失的 runtime 文件会从这里原子初始化。`generated_keys` 只接受明确声明的 `*_KEY_BASE64` / `*_PEPPER_BASE64`，仅在键缺失或仍为 `REPLACE_WITH_*` / `PENDING_*` 时生成，已有合法值不覆盖。`bindings` 的值不是模板表达式，而是 Agent 内置的有限数据源。通用来源包括 `issuer`、`client_id`、`client_secret`、`redirect_uri`、`public_url`、`tenant_id`、`application_id`、`application_code`、`environment`、`path_prefix`、`cookie_secure` 和 catalog-publisher 凭据；用途 Client 使用 `service.<purpose>.client_id` / `service.<purpose>.client_secret`。如果子系统需要新的机器用途，应先在平台控制面增加最小 scope 的用途 Client，再在清单引用，不能复用浏览器 Client。

Agent 采用“只管理声明键”的兼容策略：子系统以后新增环境变量、注释、证书或其他文件不会因为未列入清单而被删除或拒绝；需要平台生成/注入的新 runtime 文件时，只增加模板和 YAML 文件项，无需修改 Agent Go 代码。仍需严格声明的是宿主机写入目标、平台凭据映射和 Compose 服务，浏览器不能动态指定这些高权限操作。

完成上述一次性服务器初始化后，Application、Environment、登录目标、OAuth Client、运行时凭据、首次管理员授权、失败重试和安全下线都从基础平台页面操作。接入时未另选初始管理员则使用当前操作者；平台会保存这一选择，首次部署失败后的页面重试仍使用原选择，不会改授给点击重试的人。初始授权完成后，普通更新或重试不会恢复后来主动移除的角色。

日常管理员不需要登录服务器、执行子系统接入脚本或在命令行复制 OAuth 配置；服务器命令仅保留给 CI/CD、首个管理员初始化和基础设施故障恢复。部署失败时先读取环境卡片的脱敏错误与“下一步操作”，修复后点击“重试”，不要重复新增接入。

### 4.2 客户与商机接入返回 503 时

`PLATFORM_DEPENDENCY_UNAVAILABLE` 只是平台对外的安全错误码，不代表一定是 CRM API 本身故障。先在服务器检查 Agent、目标依赖和目标 API：

```bash
cd /opt/basic-platform
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml ps subsystem-provisioner customer-mysql customer-api customer-migrate
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml logs --tail 200 subsystem-provisioner customer-mysql customer-api customer-migrate
```

重点核对：

1. `platform-api` 与 `subsystem-provisioner` 必须使用同一个最新 `PLATFORM_IMAGE`，并且生产清单目录包含 `subsystems.d/customer_and_opportunity-prod.yaml`；只更新平台 API、不重建 Agent 会导致目标被旧 Agent 拒绝。
2. `.release.env` 必须包含 `CUSTOMER_CRM_IMAGE=image@sha256:<64位摘要>`；不能使用普通 tag 或占位值。
3. 不再需要手工创建 `runtime/customer.env` 或填写三个长期业务密钥。新版 Agent 会从 `subsystem-templates/customer.env.example` 初始化、自动收紧到 `0600`，并首次生成 `SENSITIVE_ENCRYPTION_KEY_BASE64`、`SENSITIVE_HMAC_KEY_BASE64`、`PORTAL_INVITE_PEPPER_BASE64`；若仍提示 runtime/模板错误，通常是 Agent 仍运行旧镜像、模板目录未随生产资产发布或 `runtime/` 不可写。
4. 若日志显示数据库、迁移或 API 健康检查失败，先处理对应容器；若显示 `deployment helper is unavailable`，先恢复 Agent；若显示 `target is not allowed`，同步完整生产部署资产并同时重建 `platform-api`、`subsystem-provisioner`。

请求响应中的追踪号会同步写入 Agent 日志的 `request_id` 字段。预检失败时控制面尚未创建应用环境，应修复后重新提交接入；只有已经生成环境并进入 `PROVISION_FAILED` 时才点击页面上的“重试”，不要重复 onboard。

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
./bin/deploy-service.sh project <image@sha256:digest>
./bin/deploy-customer-opportunity.sh <crm-image@sha256:digest> <portal-image@sha256:digest>
```

前端发布由 `compose.frontend.yaml` 独立解析和重建，只连接既有生产网络，不读取、创建或修改
`runtime/contract.env`、`runtime/project.env` 等子系统密钥文件。平台生产资产必须先于依赖它的
前端工作流同步到服务器。

脚本使用 `flock` 串行发布，校验 Compose，后端发布前备份数据库，执行迁移，更新单个服务并检查健康状态。应用失败会恢复上一镜像；已成功执行的数据库迁移不会自动反向迁移。首次发布 contract 或 project 且接入凭据仍为占位值时只保存不可变镜像指针，实际迁移和启动由基础平台页面接入触发。

CI 发布不会删除或重建 Application、Environment、LoginTarget、OAuth Client，也不会覆盖服务器 `.env`、`.release.env` 或 `runtime/*.env`；它会更新随代码审核的 `subsystems.d` 清单。仅基础平台首次接入会更新目标清单明确列出的运行时字段，其他长期业务密钥保持不变。

项目管理系统发布使用独立 `project-release` 迁移 profile，不影响既有 platform/frontend/contract 发布。首次发布 project 且 `runtime/project.env` 仍含 `PENDING_*`/`REPLACE_WITH_*` 时只安全暂存 `PROJECT_IMAGE` digest，接入完成后再由 Agent 迁移并启动 `project-api`。

客户与商机发布使用独立 `customer` Compose profile，不影响既有 platform/frontend/contract 发布。脚本先校验两个 ACR 不可变 digest，然后更新并备份 `.release.env`，并把 CRM 镜像内嵌的授权目录哈希原子写入 `runtime/customer.env`。如果 `.env`、`runtime/customer.env` 或 `runtime/portal.env` 仍含 `REPLACE_WITH_*`/`PENDING_*`，脚本只暂存镜像，不启动服务。配置完整后依次启动双库、生成一致性备份、执行两个 schema 的语句级迁移，再切换 CRM、Portal API、CRM Workers 和 Portal 邀请补偿 Worker，并检查健康/运行状态；失败时这些服务统一回滚。

测试服务器继续使用非回环 HTTP 时，在 `.env` 设置 `SUBSYSTEM_ALLOW_INSECURE_HTTP_SESSION=true`。
完成 HTTPS、Redirect URI 和 Secure Cookie 迁移后改为 `false`；该部署级开关会统一覆盖 CRM 与
Portal 的 HTTP 会话例外，清单不再永久写死允许值。

语句级迁移表为 `app_schema_migration_statements`。每条 SQL 在执行前记录 `RUNNING`，成功后才改为 `APPLIED`；重启发现遗留 `RUNNING` 会拒绝继续。必须人工核验对应表、列、索引、约束和必要回填后，按变更评审结果处理该检查点，禁止把 duplicate table/column/index 笼统视为成功。迁移器只自动接管空 schema；已有业务表但没有生产元数据表时拒绝发布，必须先建立经评审的生产基线。

### 安装或升级客户商机生产资产

将本目录的 `compose.yaml`、`compose.frontend.yaml`、完整 `subsystem-templates/`、`bin/deploy-customer-opportunity.sh` 和 Nginx 示例同步到服务器同名路径，然后执行：

```bash
cd /opt/basic-platform
install -d -m 700 runtime backups backups/releases
chmod 600 .env .release.env
chmod 750 bin/deploy-customer-opportunity.sh
```

随后从基础平台“应用接入”页面执行首次接入；Agent 会初始化缺失的 runtime 文件。已有运行配置禁止用模板覆盖，Agent 只更新清单管理键并保留新增字段。更新 Nginx 后先执行 `nginx -t`，再 reload。GitHub `test` Environment 的 `CUSTOMER_DEPLOY_SCRIPT` 必须与实际安装绝对路径一致。

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

### 6.1 Keycloak 数据库逻辑备份与恢复

生产包提供两项不改变网络入口、不会强制 HTTPS 的运维资产：

- `bin/backup-keycloak-mysql.sh`：在线一致性逻辑备份，校验 gzip、非空文件和 SHA-256；默认不删除历史备份。
- `bin/restore-keycloak-mysql.sh`：先做备份完整性、校验和、Compose、数据库健康与凭据占位符检查；真正导入需要显式破坏性确认，且拒绝在 Keycloak 仍运行时执行。

安装时仅需收紧目录和脚本权限；不要把备份、`.env` 或 runtime Secret 提交到 Git：

```bash
cd /opt/basic-platform
install -d -m 700 backups/keycloak monitoring/textfile
chmod 750 bin/backup-keycloak-mysql.sh bin/restore-keycloak-mysql.sh
chmod 600 .env .release.env
```

备份命令可以在线执行。它在 `keycloak-db` 容器内读取数据库 root 密码，因此密码不会出现在宿主机命令行、cron 参数或日志中：

```bash
cd /opt/basic-platform
./bin/backup-keycloak-mysql.sh
```

恢复必须先在**隔离演练环境**验证。先只校验备份，不会修改数据：

```bash
cd /opt/basic-platform
./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-YYYYMMDDTHHMMSSZ.sql.gz \
  --verify-only
```

获批的实际恢复是破坏性操作：先冻结认证变更、停止全部 Keycloak 节点，并确保已准备同一时间点的受控 Secret、入口配置和镜像 digest。脚本不会自行停止或启动容器，避免在未知拓扑中误操作；只有输入固定确认文本才会清空并导入独立 Keycloak 数据库：

```bash
cd /opt/basic-platform
# 仅在已完成隔离演练并获得变更审批后执行：
./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-YYYYMMDDTHHMMSSZ.sql.gz \
  --confirm RESTORE_KEYCLOAK_DATABASE
```

导入成功不代表恢复完成。恢复后使用备份匹配的受控配置启动经批准的 Keycloak 镜像，再验证数据库健康、`/health/ready`、Issuer discovery、Realm、Client、JWKS 和一次测试账号登录；同时记录实际 RTO/RPO。脚本只接受 `backups/keycloak` 目录中的 `keycloak-*.sql.gz`，默认拒绝符号链接与未停止 Keycloak 时的导入。
