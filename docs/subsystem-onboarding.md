# 统一登录目标脚本配置操作说明

> 更新日期：2026-07-28
> 配置方式：通过 `scripts/subsystem-onboarding.sh` 接入、通过 `scripts/subsystem-offboarding.sh` 撤销单个环境；基础平台前端不展示统一登录目标配置。

“统一登录目标”属于基础设施接入配置，当前**不在基础平台前端展示**。新增子系统统一使用：

```bash
bash scripts/subsystem-onboarding.sh [认证参数] [接入参数]
```

脚本调用 `POST /api/v1/subsystem-onboarding`。当应用编码尚不存在时，后端会创建 Application、Environment、相对路径 LoginTarget 和 OAuth Client；当同一租户下的应用编码已存在但指定环境尚不存在时，只复用既有 Application 并创建该环境及其 LoginTarget、OAuth Client。脚本不会覆盖已有环境、登录目标或客户端，并把生成的 OIDC 密钥直接交给受控 provisioner。浏览器和脚本输出都不会返回 Client Secret。

## 核心边界：接入配置与子系统发布解耦

统一登录接入属于基础平台控制面配置；子系统代码、镜像、前端、后端、功能模块和业务数据库迁移属于子系统发布面。两者必须解耦：

| 场景 | 是否运行接入/撤销脚本 | 正确操作 |
| --- | --- | --- |
| 首次接入一个尚不存在的应用环境 | 运行 `subsystem-onboarding.sh` | 创建该环境的统一登录配置 |
| 常规代码、镜像、前端/后端功能模块更新 | **不运行** | 仅执行子系统自身的构建、迁移、发布或重启流程 |
| 子系统容器重启、重新部署或回滚 | **不运行** | 仅操作子系统运行时；基础平台中已有接入配置保持不变 |
| 变更 BaseURL、UpstreamURL、PathPrefix、OAuth 回调 | 不用撤销重建 | 走基础平台受控配置变更流程，并同步运行时配置 |
| 某个环境永久下线 | 运行 `subsystem-offboarding.sh` | 在完成数据保留与会话处置后永久撤销该环境接入 |

> **禁止事项：** 不得为了子系统的日常更新、镜像重建、功能迭代或故障恢复而先撤销再重新接入。若误执行接入脚本并收到 `IAM_SUBSYSTEM_ALREADY_ONBOARDED`（HTTP 409），表示现有接入已受保护且没有被覆盖；停止重复执行即可。

## 1. 前置条件

- 基础平台 API 已启动；本地默认地址为 `http://127.0.0.1:8081/api/v1`。
- 运行环境已启用受控子系统 provisioner。本地 `compose.local.yaml` 已启用；生产环境必须先部署并启用对应 provisioner，不能只部署 API 后直接执行脚本。
- `--application-code` 对应的子系统项目目录已存在于 provisioner 配置的项目根目录中。
- 执行账号具备以下权限：
  - `platform:application:create`
  - `platform:application-environment:create`
  - `platform:application-login-target:create`
  - `platform:oauth-client:create`

撤销单个既有环境还需要：

- `platform:application-environment:delete`

## 2. 参数映射

| 脚本参数 | 后端字段 | 含义 |
| --- | --- | --- |
| `--application-code` | `Application.Code` | 子系统唯一编码；同时参与项目目录定位和 Client ID 派生 |
| `--application-name` | `Application.Name` | 门户显示名称 |
| `--description` | `Application.Description` | 应用说明；默认 `门户路径接入：<path-prefix>` |
| `--environment` | `Environment.Environment` | `dev`、`test`、`staging` 或 `prod`，默认 `prod` |
| `--public-base-url` | `Environment.BaseURL` | 用户访问的门户 origin，不包含业务路径 |
| `--upstream-url` | `Environment.UpstreamURL` | 门户 Nginx/容器能够访问的子系统内部地址 |
| `--path-prefix` | `Environment.PathPrefix` | 门户路径前缀，默认 `/<application-code>` |
| `--client-type` | `OAuthClient.ClientType` | `confidential` 或 `public`，默认 `confidential` |

后端自动派生，不需要额外参数：

```text
LoginTarget.TargetURI = <path-prefix>/
OAuth redirect_uri     = <public-base-url><path-prefix>/auth/callback
OAuth client_id        = <application-code>-<environment>-web
```

`BaseURL` 与 `UpstreamURL` 必须解耦：前者是浏览器看到的统一入口，后者是网关访问的内部地址。不要把容器内的 `localhost` 当作其他容器的 Upstream。

## 3. 交互向导、快捷预设与预检

### 3.1 中文交互向导

在终端中直接运行即可进入中文配置向导；如果只填写了一部分必填参数，脚本也会自动补问缺失项：

```bash
cd /Users/yglf/GOPATH/src/Unified_Identity_Authentication_Platform/platform
bash scripts/subsystem-onboarding.sh
```

向导会逐项校验应用编码、环境、对外 BaseURL、内部 UpstreamURL 和路径前缀，最后回显**不含密码、Cookie 和 Client Secret**的配置摘要。输入 `y` 或 `yes` 后才会登录并创建接入。

即使参数已完整，仍可强制进入向导核对：

```bash
bash scripts/subsystem-onboarding.sh --interactive --account admin
```

### 3.2 合同管理本地快捷配置

`contract-management-local` 预设填充本地合同系统的常用配置：

```text
应用编码：contract_management
应用名称：合同管理系统
环境：prod
对外 BaseURL：http://localhost:8081
内部 UpstreamURL：http://contract-api:8081
路径前缀：/contract_management
```

执行：

```bash
bash scripts/subsystem-onboarding.sh \
  --preset contract-management-local \
  --account admin
```

脚本仍会在创建前回显摘要并要求确认。显式传入的参数优先于预设，例如可通过 `--environment staging` 覆盖预设环境。

### 3.3 仅校验、不写入（dry-run）

先检查参数和最终派生地址，不登录、不调用平台 API、不写入任何配置：

```bash
bash scripts/subsystem-onboarding.sh \
  --preset contract-management-local \
  --dry-run \
  --yes
```

`--yes` 仅跳过“最终确认”，适用于受控 CI；日常人工操作不要使用它。`--password-stdin` 不能与需要交互补问参数的向导同时使用；CI 应提供完整参数或预设及 `--account`，再从标准输入传入密码。

## 4. 本地合同管理系统示例

合同系统项目目录名为 `contract_management`，本地统一前端/API 入口为 `http://localhost:8081`，合同后端在 Compose 网络内的服务地址为 `http://contract-api:8081`。

> 合同管理系统的 `dev` 环境由数据库迁移预置，且由统一前端直接承载；它**不会**出现在 `portal-gateway.sh list` 输出中。下面命令仅用于首次新增一个尚不存在的 `prod` 环境。若 `prod` 已存在，脚本会返回 `IAM_SUBSYSTEM_ALREADY_ONBOARDED`，此时不能用重复执行脚本覆盖 BaseURL、Upstream、路径、登录目标或 OAuth Client。

```bash
cd /Users/yglf/GOPATH/src/Unified_Identity_Authentication_Platform/platform

bash scripts/subsystem-onboarding.sh \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --environment prod \
  --public-base-url http://localhost:8081 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management \
  --client-type confidential \
  --account admin
```

脚本会安全交互读取密码。用于受控 CI 时可以从标准输入传入：

```bash
printf '%s\n' "$PLATFORM_ADMIN_PASSWORD" | bash scripts/subsystem-onboarding.sh \
  --password-stdin \
  --account admin \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --public-base-url http://localhost:8081 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management
```

也可通过 `--cookie-file FILE` 复用已有平台会话。脚本会把 Cookie 复制到私有临时目录，不覆盖调用者文件。

## 5. 单终端登录注意事项

平台禁止同一账号同时保持多个终端会话。脚本使用账号口令登录时，会在执行结束后自动调用 `/auth/logout`，避免运维脚本遗留会话导致管理员之后无法登录。

如果管理员账号已有会话，优先在原终端正常退出。只有明确要撤销原会话时才使用：

```bash
--replace-existing-session
```

该参数会使原终端会话立即失效，不应作为默认配置。

## 6. 与 portal-gateway.sh 的边界

`scripts/portal-gateway.sh` 是低层 Nginx 路由维护工具，只处理路径到 Upstream 的映射，不创建 Application、Environment、LoginTarget 或 OAuth Client。

新增子系统不要只执行 `portal-gateway.sh add`；应执行 `subsystem-onboarding.sh`，让后端受控 provisioner 完成配置写入、子系统启动和网关更新。`portal-gateway.sh` 仅用于故障排查、删除路由或受控运维。

## 7. 常用检查与冲突处理

```bash
bash scripts/subsystem-onboarding.sh --help
bash scripts/docker-local.sh ps
```

`portal-gateway.sh list` 仅列出需要独立 Nginx 整站代理的外部子系统；`contract_management` 已集成到统一前端，**不应**要求它出现在该列表中。合同系统入口固定为：

```text
http://localhost:8081/contract_management/
```

脚本是创建/新增环境流程，不是更新或覆盖流程。相同租户下已有应用编码时，若指定环境尚不存在，脚本会复用该 Application 并新增该环境。重复使用相同的应用编码和环境时，平台会返回 `IAM_SUBSYSTEM_ALREADY_ONBOARDED`（HTTP 409）及应用、环境和状态；不会覆盖任何现有配置或重新生成/泄露 OAuth Secret。需要修改现有接入配置时，应使用受控的 Application Environment、LoginTarget 或 OAuth Client 更新接口，不能通过重复执行接入脚本绕过唯一性约束。


## 8. 撤销单个环境（保留 Application 与 dev）

当只需要删除 `contract_management/prod` 的统一登录接入时，使用环境级撤销脚本：

```bash
cd /Users/yglf/GOPATH/src/Unified_Identity_Authentication_Platform/platform

bash scripts/subsystem-offboarding.sh \
  --application-code contract_management \
  --environment prod \
  --confirm contract_management/prod \
  --account admin
```

`--account admin` 指的是**基础平台管理员账号**，用于调用基础平台 API；它不是合同管理系统的业务账号。执行账号必须具备 `platform:application-environment:delete` 权限。脚本使用应用编码和环境查询当前版本，再调用：

```http
DELETE /api/v1/applications/{application_id}/environments/{environment_id}
```

`--confirm` 是强制二次确认，必须精确写成 `<application-code>/<environment>`。对本例只能是 `contract_management/prod`；不能省略、不能写成其他环境。

成功后，平台会在一个事务内删除**这个环境派生的** LoginTarget、OAuth Client 及其授权码、刷新令牌、客户端凭据、重定向 URI、JWK 等关联配置，并删除目标 Environment。下列内容不会删除：

- `contract_management` Application；
- 其他环境，特别是预置的 `contract_management/dev`；
- Docker 容器、镜像、Nginx 进程或统一前端；
- 合同、客户、审批等子系统业务数据；
- 配置命名空间和审计回执。

安全限制：

- 不允许删除 `dev`，也不能删除基础平台 `platform` 的环境；
- 不提供 `--force`；若返回 `IAM_ENVIRONMENT_DELETE_BLOCKED`（HTTP 409），说明该环境仍存在配置命名空间或审计回执，必须按数据保留策略处理，脚本不会销毁这些记录；
- 脚本不会调用 `portal-gateway.sh`、Docker 或 Nginx。对于由统一前端承载的合同系统，通常无需额外删除网关路由；
- 已发出的浏览器会话或令牌应按原有会话/令牌到期策略失效。需要立即切断用户访问时，应先在平台侧注销相关会话，再执行撤销。

只有在该环境已经**永久下线**、并在后续被当作一个新环境重新投入使用时，才使用第 3 节的交互向导或第 4 节的完整示例重新接入。不得为了子系统代码更新、镜像重建、功能模块迭代、容器重启或日常发布而撤销后重建。部署基础平台时需先应用迁移 `000053_add_application_environment_delete_permission.sql`，否则管理员不会获得此环境删除权限。

## 9. 接入与撤销故障处理

| 现象 | 原因 | 处理方式 |
| --- | --- | --- |
| `AUTH_CONCURRENT_SESSION` | 平台管理员账号已有有效会话 | 先在原终端退出；只有明确要替换时才传入 `--replace-existing-session` |
| API 连接失败 | API 地址错误、服务未启动或网络不可达 | 脚本会提示检查 `bash scripts/docker-local.sh ps`；确认 `--api-base-url` 指向正确基础平台 API |
| HTTP `401`（接入脚本） | 管理员密码错误、Cookie 失效或基础平台地址不一致 | 改用 `--account` 重新认证；不要复用过期 Cookie |
| HTTP `403`（接入脚本） | 账号缺少应用、环境、登录目标或 OAuth Client 创建权限 | 由超级管理员补齐接入权限后重试 |
| HTTP `409` / `IAM_SUBSYSTEM_ALREADY_ONBOARDED` | 同一应用和环境已存在 | 这是保护现有接入的正常结果；子系统日常发布无需执行接入脚本。仅在 BaseURL、UpstreamURL、PathPrefix 或 OAuth 回调确需变更时走受控配置变更流程；不要为常规更新撤销后重建 |
| HTTP `409` / `IAM_CONFLICT` | Client ID、路径前缀或环境资源冲突 | 核对应用编码、环境、路径前缀；不要重复创建覆盖 |
| HTTP `422`（接入脚本） | BaseURL、UpstreamURL、路径前缀或环境不合法 | 对外 BaseURL 只写 origin；Upstream 使用网关可达内网地址；PathPrefix 必须是非根绝对路径 |
| HTTP `5xx` / `PLATFORM_DEPENDENCY_UNAVAILABLE` | 基础平台 API 或受控 provisioner 不可用 | 执行 `bash scripts/docker-local.sh ps` 和 `bash scripts/docker-local.sh logs api subsystem-provisioner`；后端更新后执行 `bash scripts/docker-local.sh refresh-api` |
| HTTP `405` / `PLATFORM_METHOD_NOT_ALLOWED` | 运行中的 `api` 容器仍是未注册 `DELETE /api/v1/applications/{application_id}/environments/{environment_id}` 的旧镜像 | 在 `platform` 目录执行 `bash scripts/docker-local.sh refresh-api`；该命令只更新基础平台 API、受控 provisioner 和基础平台迁移，不会构建前端或合同后端。成功后重新执行撤销命令 |
| HTTP `403` | 账号缺少 `platform:application-environment:delete` | 为基础平台管理员授予环境删除权限，并确认迁移 000053 已执行 |
| HTTP `409` / `IAM_ENVIRONMENT_DELETE_BLOCKED` | 环境关联了配置命名空间或审计回执 | 按保留与审计流程处理；没有 `--force`，不要直接删库 |
| HTTP `409` / `IAM_VERSION_CONFLICT` | 查询后环境版本已变更 | 重新执行脚本，读取新版本后再次明确确认 |
| 未找到应用或环境 | `contract_management/prod` 本来不存在或输入不精确 | 核对应用编码和环境；脚本不会执行删除 |
| 参数校验失败 | 尝试删除 `dev`、`platform` 或确认字符串不匹配 | 仅对 test、staging、prod 使用与目标完全一致的 `--confirm` |
