# 子系统开发与统一身份接入手册

> 更新日期：2026-08-03
> 适用对象：子系统开发人员、联调人员、平台管理员和部署人员。
> 推荐入口：基础平台“应用接入”页面。生产环境的首次接入、重试、运行时更新和下线均从页面完成；`scripts/subsystem.sh` 仅保留给本地自动化、无人值守和故障排查。

## 1. 先理解三个边界

### 1.1 首次接入不等于日常发布

统一登录接入属于平台控制面；子系统代码、镜像、数据库迁移和容器重启属于发布面。

| 场景 | 正确操作 |
| --- | --- |
| 首次创建一个不存在的应用环境 | 基础平台“应用接入 → 新增接入” |
| 更新代码、镜像、前后端模块或业务迁移 | 使用子系统自己的 CI/CD，不执行 onboard/offboard |
| 生产环境只需按现有配置重建子系统 | 基础平台页面“更新运行时” |
| 本地环境只需重建现有子系统容器 | `subsystem.sh update`，或子系统自己的部署命令 |
| 首次接入或重建因 Agent 故障失败 | 页面查看部署状态并点击“重试”；不要重复新增接入 |
| 修改 BaseURL、Upstream、PathPrefix 或 OAuth 回调 | 先走受控管理 API，并同步子系统运行配置；不要撤销重建 |
| 环境永久下线 | 完成数据、会话和恢复预案后在页面确认“下线并删除控制面记录” |

收到 HTTP `409 IAM_SUBSYSTEM_ALREADY_ONBOARDED` 表示现有接入受到保护，没有被覆盖。不要为了日常发布先下线再重新接入。

### 1.2 部署 Agent 是什么

本文中的 **部署 Agent** 是平台自带的普通后台程序 `subsystem-provisioner`，不是 AI。它运行在
API 进程之外，通过受限 Unix Socket 接收经过鉴权的部署请求，负责写入受控配置、执行 Docker
Compose、维护门户网关并执行必要的部署后动作。API 不直接挂载 Docker Socket，浏览器也不能直接
调用 Agent。

平台为每个应用环境持久化部署状态：

| 状态 | 含义 | 门户是否展示 |
| --- | --- | --- |
| `PROVISIONING` | 首次接入已登记，Agent 正在部署 | 否 |
| `UPDATING` | 正在重建或重试 | 否 |
| `VERIFYING` | 预留的健康验证阶段 | 否 |
| `READY` | 最近一次 Agent 操作成功 | 是，但仍需满足用户授权 |
| `PROVISION_FAILED` | 最近一次 Agent 操作失败 | 否，可重试 |
| `DRAINING` | 正在下线 | 否 |
| `OFFBOARDED` | 运行时已拆解 | 否 |

数据库中的 Application、Environment 和 OAuth Client 表示“控制面已登记”；`READY` 表示“部署面已
完成”。两者不再混为一个状态。失败记录不会保存 Secret、命令行或完整容器日志。

### 1.3 三类 URL 不能混用

| 名称 | 使用者 | 示例 |
| --- | --- | --- |
| 平台管理 API | 接入脚本向平台登录和写控制面数据 | `http://localhost:8081/api/v1` |
| Public BaseURL | 用户浏览器访问统一门户的 origin | `http://localhost:8081` |
| UpstreamURL | 门户网关在 Docker/内网中访问子系统 | `http://customer-api:8080` |

容器中的 `localhost` 只表示当前容器。不要将 `http://localhost:8080` 作为另一个容器的 Upstream。

当前仓库内置的客户与商机管理系统是统一 Docker 编排的特例，必须使用以下固定值；管理页面选择其应用编码后会自动填写：

| 字段 | 固定值 |
| --- | --- |
| ApplicationCode | `customer_and_opportunity` |
| UpstreamURL | `http://customer-api:8090` |
| PathPrefix | `/customer-opportunity` |
| 本地访问地址 | `http://localhost:8081/customer-opportunity/` |

早期原型值 `http://opportunity-api:8082` 与 `/customer_and_opportunity` 会由当前后端兼容转换为上述规范值，但新接入不应继续使用旧值。该应用没有独立 Compose 文件：部署 Agent 会调用 `platform/compose.local.yaml` 中的 `customer-mysql`、`customer-migrate`、`customer-api`，一次性发布内嵌权限目录后再启动 OIDC 模式。

客户自助门户也是统一编排的受控特例，但它是面向外部客户的独立应用、独立后端进程和独立数据库：

| 字段 | 固定值 |
| --- | --- |
| ApplicationCode | `customer_portal` |
| UpstreamURL | `http://portal-api:8091` |
| PathPrefix | `/customer-portal` |
| 本地访问地址 | `http://localhost:8081/customer-portal/` |

Portal 与 CRM 复用 `customer_and_opportunity` 源码仓库，但 Dockerfile 分别构建 `customer-opportunity/backend:local`（`crm-runtime`）和 `customer-portal/backend:local`（`portal-runtime`），不能复用后端镜像、浏览器 OIDC Client、数据库、Cookie 或机器 Client。部署 Agent 会启动 `portal-mysql`、运行 Portal 专用 migration、发布 Portal 目录、启动独立 `portal-api`，再重建 `customer-api` 使邀请服务取得最新最小权限凭据。

### 1.4 浏览器 OAuth Client 与服务客户端分离

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
  - `platform:role-binding:update`
- 角色管理权限不会自动代表可以授予超级管理员；初始管理员授权仍受平台的受保护角色与可委派权限策略约束。

正式生产使用 `deploy/production/compose.yaml` 时同样从基础平台页面接入。生产编排中的隔离 Agent 只处理服务器 `subsystems.d/*.yaml` 审核清单内的精确应用/环境；当前随包提供合同管理、客户与商机管理和客户自助门户三个 `prod` 目标。Agent 会从清单指定的审核模板初始化缺失的 `runtime/*.env`、把权限自动收紧为 `0600`、首次生成声明的业务 base64 密钥，再写入一次性 OIDC、目录发布及用途隔离的服务凭据，最后执行固定备份、迁移和 API 重建。已有合法密钥、子系统新增环境变量和清单外文件都会保留，重试或更新不会轮换。管理员不需要在命令行配置或复制 Secret；平台 API 本身仍不挂载 Docker Socket。

新增生产子系统时，由部署人员在平台生产资产中提交并评审一个清单，同时准备 Compose 服务、运行时模板和不可变镜像键；发布后管理页面自动显示该目标。无需在每台服务器手工添加应用白名单，也不需要修改 Agent Go 代码。以后新增普通文件或环境变量不会被 Agent 拒绝；只有需要 Agent 创建/注入的新 runtime 文件或平台凭据才加入清单。清单不支持命令、脚本或任意宿主机路径，平台 API 的能力列表只是 UI 提示，特权 Agent 会独立重新加载并校验同一只读清单。详细格式和安全边界见 [`deploy/production/README.md`](../deploy/production/README.md#41-新增生产子系统目标部署人员)。

接入 API 可指定 `initial_admin_user_id`；当前生产管理页面默认使用当前平台操作者。平台将该选择持久化到部署状态：如果首次部署在初始授权前失败，页面“重试”仍向原选择用户授权，而不是改授给点击重试的人；初始授权完成后，普通更新或重试不会恢复管理员后来主动移除的角色。`customer_portal` 等按外部邀请预配身份的受控应用可不建立内部初始管理员。

镜像仓库凭据、平台密钥、数据库、Docker/Compose、生产部署目录和隔离 Agent 是一次性基础设施初始化，由部署人员或 CI/CD 完成。完成后，日常平台管理员只使用“应用接入”页面进行首次接入、查看状态、按页面“下一步操作”排障、重试、更新和下线，不登录服务器，也不手工复制 OAuth Client ID/Secret。

### 4.2 本地脚本参数（生产页面不需要输入这些命令）

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

### 4.4 客户自助门户本地快速路径

先确认平台、CRM 和受控部署 Agent 正常：

```bash
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh logs api subsystem-provisioner customer-api
```

仅当平台中不存在 `customer_portal/dev` 时执行 dry-run：

```bash
bash scripts/subsystem.sh onboard \
  --preset customer-portal-local \
  --api-base-url http://localhost:8081/api/v1 \
  --platform-origin http://localhost:8081 \
  --account admins \
  --dry-run --yes
```

摘要必须是：

```text
Application    customer_portal
Environment    dev
Public URL     http://localhost:8081/customer-portal/
OAuth Client   customer_portal-dev-web
Redirect URI   http://localhost:8081/customer-portal/auth/callback
Upstream       http://portal-api:8091
初始管理员     不授予内部管理员；由 CRM 邀请流程开通外部客户
```

确认后去掉 `--dry-run --yes` 正式执行。Agent 额外创建六个独立服务 Client，每个只能持有一个精确 scope：

| 用途 | Scope |
| --- | --- |
| 基础平台外部用户预置 | `external_user.provision` |
| 分配 Portal 角色 | `application_role.assign` |
| 回收 Portal 角色 | `application_role.revoke` |
| 创建 Portal 身份映射 | `portal.identity_mapping.provision` |
| 禁用 Portal 身份映射 | `portal.identity_mapping.disable` |
| Portal 校验 CRM 邀请 | `portal.invite.verify` |

这些 Client Secret 只交给 Agent，分别写入 Portal/CRM 运行配置，不回显到管理页面，不复用浏览器或目录发布凭据。接入成功后：

1. CRM 有权管理员打开客户详情的“门户访问”，选择唯一登记联系人并生成邀请；
2. 平台原子预置外部用户、无密码登录账号，并分配 `customer_portal/portal_customer`；
3. Portal 建立 `OIDC sub ↔ customer/contact` 映射；CRM 返回登录账号和一次性激活链接；
4. 平台管理员在“系统设置 → 登录账号”查找该账号，点击“初始化密码”；临时密码仅显示一次；
5. 通过受控渠道把登录账号、临时密码和邀请链接交付联系人；CRM/Portal 永不保存明文密码；
6. 客户打开邀请链接，经基础平台 OIDC 登录后消费邀请并进入 Portal。

平台 migration `000071` 还会为升级前已预置的外部身份补齐登录账号，并通过 `(tenant_id, login_account_id)` 外键和唯一键固定“一个外部身份对应一个平台账号”。该迁移不会生成密码凭据；密码初始化仍必须由有权平台管理员显式执行。

> 当前本地容器运行到平台/CRM 健康并不代表 Portal 已接入。只有应用接入成功、部署状态为 `READY` 且 `/customer-portal/healthz` 返回 200，才表示 Portal 可用。若浏览器会话已过期，先登录基础平台再执行接入；不要绕过管理权限直接向数据库写 Application 或 OAuth Secret。

已有 `customer_portal/dev` 不要重新 onboard，日常更新执行：

```bash
bash scripts/docker-local.sh refresh-portal-api
bash scripts/docker-local.sh refresh-frontend
```

### 4.5 通用新子系统先 dry-run

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

### 4.6 正式执行

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

### 4.7 接入期间发生什么

1. 基础平台页面校验并显示配置摘要；本地脚本模式提供等价的交互校验。
2. 以当前平台管理员会话调用 `POST /api/v1/subsystem-onboarding`。
3. 平台原子创建 Application、Environment、LoginTarget、浏览器 OAuth Client 和独立目录发布 Client；部署 Agent 发布角色目录后再为初始管理员建立应用角色授权。采用规范 `admin` 角色的子系统分配 `admin`；内置客户与商机系统按其 `max_effective_roles=3` 策略分配 `sales_director + team_lead + technical_lead`。`customer_portal` 不把内部平台操作者设为外部客户角色，访问必须来自 CRM 邀请链路。
4. 平台同时写入 `PROVISIONING` 部署状态；在 Agent 成功前，门户不会展示该环境。
5. Client Secret 只在后端内存中交给部署 Agent，不回显给浏览器或脚本。
6. Agent 基于 `.env.example` 写出权限为 `0600` 的 `.env.local`。
7. 执行子系统 Compose `up -d --build`。
8. 更新门户网关、执行 `nginx -t` 并 reload。
9. 同步目标子系统授权目录。对需要内部初始管理员的受控应用，目录缺失会使初始授权无法完成，部署记录进入 `PROVISION_FAILED`；修复 Agent、发布凭据或子系统目录后，应在原记录上点击“重试”，不能重复新增接入。按外部邀请预配身份且无需内部管理员的应用可按自身策略处理目录发布结果。
10. 成功后状态切换为 `READY`；失败时记录为 `PROVISION_FAILED`。
11. 页面继续使用当前平台会话；本地脚本会注销自己创建的临时平台会话。

若 Agent 阶段失败，控制面记录会保留，这是为了避免重新签发 OAuth Client 和丢失仅交付一次的
Secret。生产环境不要重复新增接入：在环境卡片读取脱敏错误与“下一步操作”，修复所指向的一次性基础设施问题后点击“重试”。`subsystem-status` 返回的 `next_action` 与页面提示一致，不包含可执行命令或 Secret。

以下命令只用于本地开发或服务器故障排查，不是生产日常管理员流程：

```bash
bash scripts/subsystem.sh status \
  --application-code customer_management \
  --environment dev \
  --account admins

bash scripts/docker-local.sh logs api subsystem-provisioner

bash scripts/subsystem.sh retry \
  --application-code customer_management \
  --environment dev \
  --account admins
```

`retry` 只重新执行现有环境的部署 Agent 流程，不重新创建 Application、Environment、登录目标或
OAuth Client。每次重试都会增加 `generation` 和 `attempt_count`，便于审计和排障。首次授权未完成时，重试沿用接入时保存的初始管理员；已完成时不会重复授予或恢复后来被主动移除的角色。

## 5. 接入后验收清单

```bash
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh logs api subsystem-provisioner
bash scripts/portal-gateway.sh list
bash scripts/subsystem.sh status \
  --application-code customer_management \
  --environment dev \
  --account admins
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
执行期间部署状态为 `UPDATING`，门户暂时隐藏该环境；成功后恢复为 `READY`。失败后使用 `status`
查看安全错误摘要，修复 Docker、网络、Compose 或 Agent 配置后使用 `retry`，不要重新 onboard。

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
| `status=PROVISION_FAILED` | 生产在页面读取 `next_action`，修复后点击“重试”；本地/排障可使用 `subsystem.sh status/retry`，不要重复 onboard |
| `subsystem project ... unavailable` | 应用编码与目录名不一致，或缺 Compose/`.env.example` |
| Compose 启动失败 | 在子系统目录用生成的 `.env.local` 单独执行 `docker compose config` 和 `up` 排查 |
| `nginx -t` 失败 | 检查 path prefix、Upstream、include；不要绕过验证直接 reload |
| 等待网关锁超时 | 查找并发 CI/provisioner；确认没有活跃进程后再处理遗留目录锁 |
| OAuth `redirect_uri` 不匹配 | 浏览器请求值、平台登记值和子系统配置必须逐字符一致 |
| 登录后循环跳转 | 检查 Cookie Secure/SameSite、代理头、issuer、路径前缀和回调会话持久化 |
| 目录同步失败 | 查看 provisioner stderr；核对 publisher、MySQL 坐标、应用 ULID、curl/jq |
| offboard 后 DB 删除失败 | 按第 9 节恢复运行时或完成剩余删除，禁止重新 onboard 覆盖 |

保留平台响应中的 `request_id`，用它关联 API、审计和 provisioner 日志。
