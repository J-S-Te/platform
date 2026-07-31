# 子系统开发与统一身份接入手册

> 更新日期：2026-07-31
> 适用对象：子系统开发人员、联调人员、平台管理员和部署人员。
> 官方入口：`scripts/subsystem.sh`；兼容入口 `subsystem-onboarding.sh`、`subsystem-offboarding.sh` 仍可使用。

## 1. 先理解三个边界

### 1.1 首次接入不等于日常发布

统一登录接入属于平台控制面；子系统代码、镜像、数据库迁移和容器重启属于发布面。

| 场景 | 正确操作 |
| --- | --- |
| 首次创建一个不存在的应用环境 | `subsystem.sh onboard` |
| 更新代码、镜像、前后端模块或业务迁移 | 使用子系统自己的 CI/CD，不执行 onboard/offboard |
| 只需要重建现有子系统容器 | `subsystem.sh update`，或子系统自己的部署命令 |
| 修改 BaseURL、Upstream、PathPrefix 或 OAuth 回调 | 先走受控管理 API，并同步子系统运行配置；不要撤销重建 |
| 环境永久下线 | 完成数据、会话和恢复预案后执行 `subsystem.sh offboard` |

收到 HTTP `409 IAM_SUBSYSTEM_ALREADY_ONBOARDED` 表示现有接入受到保护，没有被覆盖。不要为了日常发布先下线再重新接入。

### 1.2 三类 URL 不能混用

| 名称 | 使用者 | 示例 |
| --- | --- | --- |
| 平台管理 API | 接入脚本向平台登录和写控制面数据 | `http://localhost:8081/api/v1` |
| Public BaseURL | 用户浏览器访问统一门户的 origin | `http://localhost:8081` |
| UpstreamURL | 门户网关在 Docker/内网中访问子系统 | `http://customer-api:8080` |

容器中的 `localhost` 只表示当前容器。不要将 `http://localhost:8080` 作为另一个容器的 Upstream。

### 1.3 浏览器 OAuth Client 与服务客户端分离

- 浏览器登录 Client：`<application-code>-<environment>-web`；通常使用 Authorization Code + PKCE。
- 授权目录发布 Client：独立的 catalog-publisher 服务凭据，不得交给浏览器。
- 审计、批处理等机器 Client：按各自用途单独创建，不能复用浏览器 Client。
- Client Secret 只能保存在权限为 `0600` 的运行配置或 Secret 管理系统中，不进入 Git、命令行参数和日志。

## 2. 子系统必须提供的项目结构

本地受控 provisioner 根据 `application-code` 在 `SUBSYSTEM_PROJECTS_ROOT` 下查找同名目录。例如：

```text
Unified_Identity_Authentication_Platform/
├── platform/
└── customer_management/       # 必须与 application-code 一致
    ├── compose.yaml            # 也支持 compose.yml/docker-compose.yml/docker-compose.yaml
    └── .env.example            # 首次接入的安全模板
```

要求：

1. 应用编码匹配 `^[a-z][a-z0-9._-]{0,63}$`，目录不能通过符号链接逃出项目根目录。
2. Compose 文件必须能执行 `docker compose --project-directory <dir> --env-file .env.local -f <file> up -d --build`。
3. 必须提供 `.env.example`；如果已有 `.env.local`，provisioner 会基于它保留未托管配置并替换平台托管字段。
4. `.env.local` 不得提交。首次接入生成后权限为 `0600`。
5. 子系统服务必须加入网关可达的 Docker 网络，且 UpstreamURL 使用该网络中的服务名和容器端口。
6. 健康检查必须覆盖数据库迁移、OIDC discovery 和必要依赖，避免容器“已启动但不可登录”。

### 2.1 provisioner 写入的标准环境变量

子系统应支持以下键；未出现在模板中的键会被追加：

| 变量 | 含义 |
| --- | --- |
| `PLATFORM_BASE_URL` | 平台配置的 OIDC issuer |
| `OIDC_ISSUER` | ID Token 的预期 issuer |
| `OIDC_CLIENT_ID` | 浏览器登录 Client ID |
| `OIDC_CLIENT_SECRET` | confidential Client Secret |
| `OIDC_REDIRECT_URI` | 精确回调地址 |
| `OIDC_SCOPES` | 当前写入 `openid profile` |
| `OIDC_TENANT_ID` | 平台租户 ID |
| `APP_PUBLIC_URL` | 用户实际进入子系统的公开地址，含路径前缀和尾斜线 |
| `APP_PATH_PREFIX` | 门户路径前缀 |
| `PLATFORM_APPLICATION_ID/CODE` | 平台应用标识 |
| `PLATFORM_ENVIRONMENT_CODE` | `dev/test/staging/prod` |
| `PLATFORM_DOCKER_NETWORK` | 平台与子系统互通的 Docker 网络 |
| `PLATFORM_AUTHORIZATION_CATALOG_*` | 授权目录发布凭据；仅后端使用 |

子系统可另外配置 `OIDC_BACKCHANNEL_BASE_URL`，让服务端 discovery/token/userinfo 走容器内地址；但必须继续验证公开 `OIDC_ISSUER`，不能把内网地址当作 Token issuer。

## 3. OIDC 客户端实现要求

### 3.1 标准流程

1. 未登录用户访问业务页。
2. 子系统生成高熵 `state`、`nonce` 和 PKCE `code_verifier`，服务端保存或以防篡改方式绑定当前浏览器会话。
3. 浏览器跳转到平台 authorization endpoint。
4. 平台只重定向到已注册的精确 `OIDC_REDIRECT_URI`。
5. 回调校验 `state`，服务端用授权码和 `code_verifier` 换 Token。
6. 校验 ID Token 签名、`iss`、`aud`、`exp`、`iat`、`nonce`；密钥只能来自受信任 issuer 的 JWKS。
7. 建立子系统会话。会话有效期不得超过 ID Token/平台授权允许的有效期；权限变化和账号禁用后必须重新验证或撤销会话。
8. 登出时撤销子系统会话；需要平台统一登出时使用平台注册的 post-logout redirect URI。

OIDC discovery：

```text
<OIDC_ISSUER>/.well-known/openid-configuration
```

不要硬编码 Token、JWKS、UserInfo 或 Logout 路径，应优先读取 discovery。

### 3.2 回调路径约定

onboard 自动派生：

```text
LoginTarget.TargetURI = <path-prefix>/
redirect_uri          = <public-base-url><path-prefix>/auth/callback
client_id             = <application-code>-<environment>-web
```

本机开发示例：

```text
Public BaseURL : http://localhost:8081
PathPrefix     : /customer_management
Redirect URI  : http://localhost:8081/customer_management/auth/callback
```

### 3.3 HTTP 回调策略

生产环境应使用 HTTPS。平台 OAuth 回调是否允许非回环 HTTP，由平台后端配置独立控制：

```dotenv
AUTH_OAUTH_CLIENT_ALLOW_INSECURE_HTTP_REDIRECT_URIS=true
```

该配置只放宽 OAuth Client 回调校验，不等同于允许接入脚本把管理员密码发送到任意 HTTP API。

平台管理 API 传输规则：

- `https://...`：允许；
- `http://localhost`、`127.0.0.0/8`、`::1`：允许；
- 其他 HTTP 主机：默认拒绝，可信局域网必须同时显式配置开关和精确主机白名单。

```dotenv
BASIC_PLATFORM_ALLOW_INSECURE_HTTP_API=true
BASIC_PLATFORM_INSECURE_HTTP_API_ALLOWED_HOSTS=192.168.3.11
```

推荐仍让脚本通过 `http://127.0.0.1:8081/api/v1` 调用平台 API，同时把 `--public-base-url` 和 `--platform-origin` 配成局域网地址。这样不需要打开非回环管理 API HTTP 例外，局域网 OAuth 回调仍正常工作。

## 4. 首次接入

### 4.1 前置条件

- 平台 API、MySQL、迁移和 `subsystem-provisioner` 正常。
- Docker daemon 可用，项目目录、Compose 和 `.env.example` 已准备好。
- 网关 include 文件可写，frontend 容器可唯一定位。
- 执行账号至少具有：
  - `platform:application:create`
  - `platform:application-environment:create`
  - `platform:application-login-target:create`
  - `platform:oauth-client:create`
- 角色管理权限不会自动代表可以授予超级管理员；初始管理员授权仍受平台的受保护角色与可委派权限策略约束。

### 4.2 参数

| 参数 | 说明 |
| --- | --- |
| `--application-code` | 唯一应用编码，同时用于项目目录定位 |
| `--application-name` | 门户展示名 |
| `--environment` | `dev/test/staging/prod`，默认 `prod` |
| `--public-base-url` | 浏览器看到的 origin，不带业务路径 |
| `--upstream-url` | 网关可达的内部 HTTP(S) 地址 |
| `--path-prefix` | 非根绝对路径，默认 `/<application-code>` |
| `--client-type` | `confidential` 或 `public` |
| `--initial-admin-user-id` | 可选；不传则使用当前平台操作者 |
| `--description` | 应用说明 |
| `--api-base-url` | 平台管理 API 根地址 |
| `--platform-origin` | Cookie 写请求 Origin，应在平台 CORS/同源策略允许范围内 |

### 4.3 合同管理本地快速路径

当前合同管理本地环境统一使用 `dev`。如果数据库中已经存在
`contract_management/dev`，不要再次接入，日常更新直接执行：

```bash
bash scripts/docker-local.sh refresh-contract-api
bash scripts/docker-local.sh refresh-frontend
```

只有首次环境不存在时才执行：

```bash
bash scripts/subsystem.sh onboard \
  --preset contract-management-local \
  --environment dev \
  --api-base-url http://localhost:8081/api/v1 \
  --platform-origin http://localhost:8081 \
  --account admins \
  --dry-run --yes
```

确认摘要中是以下值后，去掉 `--dry-run`：

```text
Application    contract_management
Environment    dev
Public URL     http://localhost:8081/contract_management/
OAuth Client   contract_management-dev-web
Redirect URI   http://localhost:8081/contract_management/auth/callback
Upstream       http://contract-api:8081
```

### 4.4 通用新子系统先 dry-run

```bash
cd /path/to/Unified_Identity_Authentication_Platform/platform

bash scripts/subsystem.sh onboard \
  --application-code customer_management \
  --application-name '客户管理系统' \
  --environment dev \
  --public-base-url http://localhost:8081 \
  --upstream-url http://customer-api:8080 \
  --path-prefix /customer_management \
  --client-type confidential \
  --api-base-url http://localhost:8081/api/v1 \
  --platform-origin http://localhost:8081 \
  --dry-run --yes
```

dry-run 不登录、不调用 API、不写 `.env.local`、不启动容器、不改网关。

### 4.5 正式执行

```bash
bash scripts/subsystem.sh onboard \
  --application-code customer_management \
  --application-name '客户管理系统' \
  --environment dev \
  --public-base-url http://localhost:8081 \
  --upstream-url http://customer-api:8080 \
  --path-prefix /customer_management \
  --client-type confidential \
  --api-base-url http://localhost:8081/api/v1 \
  --platform-origin http://localhost:8081 \
  --account admins
```

交互方式不会回显密码。CI 使用 stdin：

```bash
printf '%s\n' "$PLATFORM_ADMIN_PASSWORD" | bash scripts/subsystem.sh onboard \
  --password-stdin --yes \
  --account admins \
  --application-code customer_management \
  --application-name '客户管理系统' \
  --environment dev \
  --public-base-url http://localhost:8081 \
  --upstream-url http://customer-api:8080 \
  --path-prefix /customer_management \
  --client-type confidential \
  --api-base-url http://localhost:8081/api/v1 \
  --platform-origin http://localhost:8081
```

不要把密码写到命令行。也可用 `--cookie-file` 复制已有会话，但文件必须来自同一平台且未过期。

### 4.6 接入期间发生什么

1. 脚本校验并显示配置摘要。
2. 以平台管理员会话调用 `POST /api/v1/subsystem-onboarding`。
3. 平台原子创建 Application、Environment、LoginTarget、浏览器 OAuth Client 和独立目录发布 Client；角色目录存在时再为初始管理员建立应用角色授权。
4. Client Secret 只在后端内存中交给 provisioner，不回显给浏览器或脚本。
5. provisioner 基于 `.env.example` 写出权限为 `0600` 的 `.env.local`。
6. 执行子系统 Compose `up -d --build`。
7. 更新门户网关、执行 `nginx -t` 并 reload。
8. 对配置的目标子系统尝试同步授权目录。目录同步失败当前是非致命错误，应查看 provisioner 日志并单独修复。
9. 脚本退出前注销自己创建的平台会话。

若第 6～8 步失败，控制面记录或 `.env.local` 可能已经创建。不要盲目重复接入；先根据 request ID 和日志确认失败阶段。

## 5. 接入后验收清单

```bash
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh logs api subsystem-provisioner
bash scripts/portal-gateway.sh list
```

逐项确认：

- `.env.local` 权限为 `0600`，并已填充 OIDC Client、Tenant、Application 和路径；
- 子系统容器 healthy，能从 frontend 所在网络访问 Upstream；
- discovery 的 issuer 与 `OIDC_ISSUER` 完全一致；
- 未登录访问触发登录，回调后返回原业务页；
- 篡改/重放 state、错误 nonce、错误 issuer/audience 均被拒绝；
- 无授权用户收到 403，而不是仅靠前端隐藏菜单；
- 登出后子系统会话失效；
- 日志中没有 Client Secret、Cookie、授权码、Access Token 或完整 ID Token；
- 路径前缀下的静态资源、API、重定向和刷新均正常。

`contract_management` 已集成到统一前端，不登记整站反代，因此不应出现在 `portal-gateway.sh list` 中。

## 6. 日常更新与配置变更

### 6.1 代码/镜像更新

不执行 onboard/offboard。使用子系统 CI/CD，或者：

```bash
bash scripts/subsystem.sh update \
  --application-code customer_management \
  --environment dev \
  --account admins
```

`update` 只按现有 `.env.local` 重建 Compose；不会重写该文件，也不会修改平台 DB 或重新签发 OAuth Secret。

### 6.2 修改 BaseURL、Upstream、PathPrefix 或回调

这是一个多资源受控变更，至少涉及：

1. `PATCH /api/v1/applications/{application_id}/environments/{environment_id}`；
2. `PATCH .../login-targets/{login_target_id}`；
3. `PUT /api/v1/oauth-clients/{oauth_client_id}/redirect-uris`；
4. 必要时更新 post-logout redirect URI；
5. 安全地同步子系统 `.env.local` 中的公开 URL、路径和回调；
6. 重建子系统并验证网关。

这些接口使用乐观锁 `version`。必须先 GET 当前记录再提交新版本；发生 `IAM_VERSION_CONFLICT` 时重新读取，不要覆盖他人的变更。浏览器 OAuth Secret 不会因普通 URL 变更重新回显，不能删除 `.env.local` 后期待平台恢复明文 Secret。

## 7. 网关工具与并发锁

`portal-gateway.sh` 是底层工具，不创建平台控制面数据。正常新增子系统必须走 onboard。

```bash
bash scripts/portal-gateway.sh list
bash scripts/portal-gateway.sh add <code> <path-prefix> <upstream-url>
bash scripts/portal-gateway.sh remove <code>
bash scripts/portal-gateway.sh reload
```

`add/remove/list/sync/apply` 使用跨进程锁：

- Linux 优先使用 util-linux `flock`；
- 没有 `flock` 时使用原子目录锁；
- 默认锁文件为 `<include>.lock`，等待 60 秒；
- 可通过 `PORTAL_GATEWAY_LOCK_FILE`、`PORTAL_GATEWAY_LOCK_TIMEOUT` 调整。

不要删除一个正在使用的 `.lock` 或 `.lock.d`，也不要让多个主机通过不支持可靠文件锁的共享文件系统并发维护同一 include。

## 8. 授权目录同步

`sync-contract-catalog.sh` 只接受受保护的 `PLATFORM_*` 环境变量，不再接受命令行位置参数。它需要 `curl`、`jq`、Docker CLI，以及目标 MySQL 容器中的 mysql 客户端。

手工补偿示例（不要开启 shell tracing）：

```bash
set +x
export PLATFORM_APPLICATION_ID='<26位大写ULID>'
export PLATFORM_BASE_URL='http://127.0.0.1:8081'
export PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID='<publisher-client-id>'
export PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET='<secret>'
export PLATFORM_MYSQL_CONTAINER='basic-platform-local-mysql-1'
export PLATFORM_MYSQL_USER='basic_platform'
export PLATFORM_MYSQL_PASSWORD='<password>'
export PLATFORM_MYSQL_DATABASE='basic_platform'
bash scripts/sync-contract-catalog.sh
unset PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET PLATFORM_MYSQL_PASSWORD
```

凭据和 Bearer Token 通过进程环境与权限为 `0600` 的临时 curl 配置传递，不放入 argv。不要把这些变量写进公共 CI 输出或普通日志。

## 9. 永久下线

默认 offboard 是深清理：先停止容器、删除 `.env.local`、移除网关并 reload，再删除 Environment 控制面记录。

```bash
bash scripts/subsystem.sh offboard \
  --application-code customer_management \
  --environment prod \
  --confirm customer_management/prod \
  --account admins
```

安全边界：

- 不允许删除 `dev` 或平台自身环境；
- 不提供 `--force`；配置命名空间或审计回执仍存在时平台会拒绝；
- `--delete-application` 仅在最后一个环境删除后使用；
- `--shallow` 只用于应急修复，会保留容器、`.env.local` 和网关，容易制造漂移；
- 下线不会删除子系统业务数据库和备份，必须另走数据保留流程。

深清理分两阶段，当前不是跨 Docker/文件系统/数据库的全局事务。若运行时清理成功、DB 删除失败，会形成“平台记录仍在但服务已停止”的半状态。执行前应备份 `.env.local`、记录网关配置和 Environment version；失败后不要重新 onboard，应先恢复运行时或完成剩余删除。

## 10. 故障排查

| 现象 | 处理 |
| --- | --- |
| `AUTH_CONCURRENT_SESSION` | 先从原终端退出；确认要抢占时才用 `--replace-existing-session` |
| 非回环平台 API 必须使用 HTTPS | 改走 `127.0.0.1`/HTTPS；可信局域网才配置 HTTP 开关和精确白名单 |
| 登录接口 403 | `--platform-origin` 未被同源/CORS 策略允许，或账号被拒绝 |
| 接入接口 403 | 操作者缺少应用、环境、登录目标、OAuth Client 创建权限，或初始角色不可委派 |
| `IAM_SUBSYSTEM_ALREADY_ONBOARDED` | 环境已存在；停止重复接入，日常发布走 update/CI |
| `IAM_CONFLICT` | Client ID、路径或资源唯一性冲突，核对现有记录 |
| `PLATFORM_DEPENDENCY_UNAVAILABLE` | 查看 `api`、`subsystem-provisioner`、Docker、项目目录和 Socket |
| `subsystem project ... unavailable` | 应用编码与目录名不一致，或缺 Compose/`.env.example` |
| Compose 启动失败 | 在子系统目录用生成的 `.env.local` 单独执行 `docker compose config` 和 `up` 排查 |
| `nginx -t` 失败 | 检查 path prefix、Upstream、include；不要绕过验证直接 reload |
| 等待网关锁超时 | 查找并发 CI/provisioner；确认没有活跃进程后再处理遗留目录锁 |
| OAuth `redirect_uri` 不匹配 | 浏览器请求值、平台登记值和子系统配置必须逐字符一致 |
| 登录后循环跳转 | 检查 Cookie Secure/SameSite、代理头、issuer、路径前缀和回调会话持久化 |
| 目录同步失败 | 查看 provisioner stderr；核对 publisher、MySQL 坐标、应用 ULID、curl/jq |
| offboard 后 DB 删除失败 | 按第 9 节恢复运行时或完成剩余删除，禁止重新 onboard 覆盖 |

保留平台响应中的 `request_id`，用它关联 API、审计和 provisioner 日志。
