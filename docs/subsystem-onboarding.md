# 子系统开发与统一身份接入手册

> 源码核验日期：2026-10-22；第1—3节说明开发协议边界，第4节起区分单步目录API、遗留组合API与当前统一向导。仅静态核对，不代表部署验收通过。
> 适用对象：子系统开发人员、联调人员、平台管理员和部署人员。
> 推荐入口：基础平台“应用接入 → 统一接入向导”（第 10 节），按服务端检查和持久操作分阶段推进。旧“新增接入”仅登记目录（第 4 节）；认证、部署、商业授权及业务验收仍须分别核对。`scripts/subsystem.sh connect` 使用同组统一操作 API，旧 onboard/update/retry 保留兼容。

> **当前控制面边界**：新接入优先采用“应用目录登记 → Keycloak 集成/投影 → 运行时部署”的分层流程。兼容的 `/subsystem-onboarding` 路径仍存在于已部署环境，但不应成为新增应用的默认集成方式；目录、身份提供方 Client 和部署状态必须分别可审计。

## 1. 先理解三个边界

### 1.1 首次接入不等于日常发布

统一登录接入属于平台控制面；子系统代码、镜像、数据库迁移和容器重启属于发布面。

| 场景 | 正确操作 |
| --- | --- |
| 首次创建一个不存在的应用环境 | 优先“应用接入 → 统一接入向导”；单步新增接入只登记目录 |
| 更新代码、镜像、前后端模块或业务迁移 | 使用子系统自己的 CI/CD，不执行 onboard/offboard |
| 生产环境只需按现有配置重建子系统 | 基础平台页面“更新运行时” |
| 本地环境只需重建现有子系统容器 | `subsystem.sh update`，或子系统自己的部署命令 |
| 受控组合部署/重建因 Agent 故障失败 | 页面查看部署状态及权限后恢复；目录登记本身不调用 Agent，不重复新增 |
| 修改 BaseURL、Upstream、PathPrefix 或 OAuth 回调 | 先走受控管理 API，并同步子系统运行配置；不要撤销重建 |
| 环境退役/删除 | 按第 8 节分别停运行时、保留记录、删除或完成保留期后清理 |

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
| UpstreamURL | 门户网关在 Docker/内网中访问子系统 | CRM示例 `http://customer-api:8090`，各应用按清单 |

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

## 4. 保留的目录登记页面

### 4.1 角色、条件与步骤

当前“应用接入 → 新增接入”使用 `POST /api/v1/subsystem-directory`，需要：

- `platform:application:create`
- `platform:application-environment:create`
- `platform:application-login-target:create`

管理员选择服务器公开的应用/环境目标，填写/确认应用名、公开 BaseURL、内部 Upstream、路径前缀及 issuer alias。前端只提交目录参数；不能把页面初始管理员选择、Client 类型或自动部署当作当前行为。服务端事务创建活动的应用、环境和 home 登录目标，返回下一步动作。

该路径不调用部署 Agent、不创建 OAuth Client/Secret、不做初始管理员授权，也不创建组合部署的完整状态；可能显示 UNMANAGED。成功判断为重新查询到正确目录，不是子系统已上线。已存在目录不能重复新建，应沿用现有 ID/version。

### 4.2 登记后的认证和运行时

按 next_action 同步 Keycloak Client/角色、完成用户投影和真实 Broker 登录，观察后再切换；缺 Keycloak 管理配置、数据库/Worker/Agent 时不能靠登记消除依赖。

四门禁为 Client、角色目录、用户投影、Broker 登录；观察期 7 天，成功切换后的回滚窗口也是 7 天。当前 updateSubsystem 对 Keycloak 准备仍要求四项与观察期条件，页面仅三项的“准备”提示不能承诺可执行。细则和缺口见[Keycloak 映射与切换](keycloak-identity-mapping.md)。

运行时 Agent 在 production 模式只处理审核清单中的精确应用/环境、文件及 Compose 服务。API 权限不是任意主机命令授权；平台 API 不因登记应用就取得 Docker 权限。首次基础设施/镜像/数据库/密钥/Agent 初始化由部署方完成，本次未执行任何初始化或发布。

## 5. 遗留组合 onboard：区别于目录登记与统一向导

`POST /api/v1/subsystem-onboarding` 仍保留，除目录创建权限还需要 `platform:oauth-client:create`、`platform:role-binding:update`。CLI `scripts/subsystem.sh onboard` 属于此组合路径，不等价于 subsystem-directory。

该接口先做 Agent 预检，事务创建应用/环境/目标及独立 OAuth Client，持久化原初始管理员选择和部署状态，再启动后台部署。HTTP 返回 202、UPDATING/ONBOARD 等受理信息；不是旧文档描述的同步完成和完整资源响应。旧 CLI 对响应字段及状态的兼容性必须在目标版本核验，本次没有运行 CLI。

初始授权仅适用于该组合流程：首次未完成时重试保留原操作者/指定人，不改授点击重试的人；已完成授权不会因日常更新自动恢复后来撤销的角色。客户门户等外部邀请应用不由内部管理员选择自动获得客户映射。

Secret 在后端/Agent 受控通道交付，不回显浏览器；失败保留控制面记录，读取脱敏原因和 next_action 后恢复，不重复 onboard。后台最终 READY/PROVISION_FAILED 才表示该轮结果，READY 仍需业务登录和权限验收。

生产与本地 Agent 不同：生产基于审核 manifest 的备份/迁移/服务重建；本地存在集成 Compose 和独立子系统路径，不统一描述成所有场景 `up -d --build`。本地参数、dry-run 与命令使用交部署/开发人员参见[本地 Docker 操作](local-docker-operations.md)，不要在公共终端复制真实凭据。

## 6. 授权目录与投影验收

应用主动发布自己的版本化目录，平台展示/分配只读镜像。合同现在优先由内嵌 manifest 发布程序同步，不以从平台数据库读取的遗留 `sync-contract-catalog.sh` 为事实源。相同版本/checksum 重发不推进修订；有效变更在事务里更新目录/策略/修订并排队投影。

管理员核对目录 SYNCED、来源、版本及稳定编码，再检查岗位/人员最终授权。机器 publisher、浏览器登录 Client、审计等服务 Client 按用途分离，不复用 Secret。

见[目录发布说明](../../sharedDocs/05_能力模块/应用授权目录同步说明.md)。未完成 outbox、投影依赖失败或 Broker 门禁缺失时，不能以平台保存成功宣称目标系统权限已生效。

## 7. 日常更新、失败与恢复

业务代码/镜像发布不重复新增目录。环境配置更新、运行时更新/重试使用原应用/环境及 version，查 generation、attempt_count、operation、status 和 next_action。version 冲突刷新后再提交；不重复签发身份，不绕过审核清单。

Keycloak 环境携带完整当前网关参数进行受控更新时，会按该租户/应用/环境的有效 Client 映射重新读取现有 RP 密钥并下发运行时，修复密钥漂移；这不是轮换 Keycloak 密钥，明文也不会返回浏览器。没有网关参数的旧版纯重建/清单无变化请求仍保留原配置。出现授权码兑换 `unauthorized_client` 时，管理员可在原环境执行“更新运行时”，等待最终就绪后从门户重新发起登录，不刷新已消费的旧 callback URL。

登录回调 302 和 `/api/v1/auth/me` 200 只证明身份会话已建立。业务 API 若返回 `COMMERCIAL_LICENSE_DENIED`，需另核对本实例的有效签名许可证及运行确认；不能当作角色缺失而扩权，也不得关闭商业授权门禁。

非平台根应用的受控更新可调用 Agent；平台自身是控制面根节点，不通过子系统更新/下线按钮操作。Agent/数据库/网关失败按 request_id 定位；202 只受理、必须轮询/刷新最终状态。不得把未部署依赖归为“页面已全部可用”，也不在日志保留 Secret。

验收建议（本次未执行）：

- 公开目标、回调、内部 Upstream 三类地址各自正确；
- 目标环境部署 READY、健康检查通过；
- 真正完成 OIDC/Broker 登录、授权上下文及典型业务接口验证；
- 切换、重试后未意外恢复已撤销角色；
- Worker 与投影完成状态可核对，目录外权限失败关闭。

## 8. 退役、停运行时、删除和保留期清理

这四类操作不是同一个“永久下线”。

### 8.1 退役应用与保留环境

应用退役要求应用编码确认和 version，逻辑改为 RETIRED，保留环境、Client、登录目标和审计；不停止运行时。平台自身禁止退役。

页面“退役并保留记录”调用环境 teardown，先进入 DRAINING，Agent 成功后 OFFBOARDED，失败保留故障状态。production 实现只 compose stop 审核服务，不删除 runtime/*.env、不移除网关、不删业务库/卷。local 集成模式停止对应服务并保留配置；local 独立模式可能 down、删除 .env.local 并移除网关，不可泛化到生产。

### 8.2 删除环境与半状态

页面删除先 teardown 再删除控制面，使用精确 `application-code/environment`、最新 version。非 dev、非平台才允许环境删除。仓储存在 cfg_namespace 或 audit_ingestion_receipt 引用时阻断；不是所有引用都自动清除。

删除成功清理该环境派生平台 OAuth Client/登录目标及环境记录，不删除业务数据库/备份。运行时与数据库无全局事务，Agent 成功而 DB 拒绝时可能“服务已停、目录还在”；先读状态/引用，按恢复计划重新启用或完成合规清理，不能重新 onboard 覆盖。

### 8.3 清理退役记录的硬条件

管理员先通过审计保留期流程完成合法 PURGE，而不是手工伪造任务 ID。页面清理需要：

- 应用 RETIRED、环境持久化状态 OFFBOARDED；
- 非 dev/非平台；
- 最新 version 和精确 `PURGE/application-code/environment`；
- 保留期完成、已下线两项显式确认；
- 对应同租户、同应用的 `audit_retention_task`，mode=PURGE 且 status=COMPLETED。

仓储锁定部署状态并核查完成任务；不存在、未完成、其他租户/应用或非 PURGE 均不能靠输入框绕过。成功清理环境的 Client、登录目标、服务实例/部署状态、配置发布/命名空间和审计回执等控制面关联，不在这个接口直接删除 audit_event，也不销毁子系统业务数据库、磁盘持久化文件或备份。保留期任务本身的范围须交审计负责人核实。

## 9. 故障判断

| 现象 | 处理 |
| --- | --- |
| 登记成功但没有服务/凭据 | 目录登记不是部署；按认证集成与运行时下一步处理 |
| 403 | 查对应目录/更新/切换权限和机器应用边界，不复用其他 Client |
| 409/version 冲突 | 刷新目录及状态后重试，不并发反复操作 |
| 202 | 仅受理，读取最终状态并做业务登录验收 |
| Keycloak 门禁或观察阻断 | 核对当前 Client hash、outbox、真实 Broker 证据和时间窗；不伪造通过 |
| PROVISION_FAILED | 修复审核清单/基础设施/凭据发布/Agent 依赖，沿原记录恢复 |
| teardown 成功、环境删除失败 | 核实配置/审计回执阻断和实际运行时，按半状态计划恢复 |
| PURGE 被拒绝 | 核实应用/环境终态、确认码、version 和同租户同应用已完成 PURGE 任务 |

源码依据及原陈述纠正见[平台核验记录](../../.ai/modules/operation-docs-platform-20261013.md)。本次没有运行发布、停止、迁移、清理、登录或外部接口。

## 10. 统一接入向导（六个子系统共用入口）

本节描述统一入口的**设计与当前落地边界**；旧路径（`subsystem-directory`、组合 `subsystem-onboarding`、CLI `onboard/update/retry`）保留不变，见第 4、5、7 节。

### 10.1 接口与职责

| 接口 | 方法 | 作用 |
|---|---|---|
| `/api/v1/subsystem-access/preview` | POST | **只读**条件检查：返回服务端判定的动作、起始阶段、缺项（含下一步）、所需权限、主体缺少的权限、变更摘要与 `preview_version` |
| `/api/v1/subsystem-access/operations` | POST | 确认执行：返回 `202` 与持久化操作 ID；**必须携带 `preview_version`**，事实变化返回 `409` 并要求重新预览 |
| `/api/v1/subsystem-access/operations/:id` | GET | 查询操作：状态、当前阶段与**分项阶段结果**（阶段·状态·尝试次数·证据引用） |
| `/api/v1/subsystem-access/operations?application_code=&environment=` | GET | 按目标定位最近一次操作（页面刷新、CLI 重连） |
| `/api/v1/subsystem-access/operations/:id/resume` | POST | 条件补齐后继续：服务端**重新检查**事实，满足才放回可执行 |

动作由服务端判定（`ONBOARD` 首次接入 / `ADOPT` 接管运行时 / `UPDATE` 受控更新 / `RETRY` 重试失败部署 / `NONE` 已一致仅展示），**页面与 CLI 都不选择动作**。

### 10.2 状态、等待与并发

- 状态：`PENDING → RUNNING → SUCCEEDED / FAILED / WAITING`；
- **等待处理**（`WAITING`）：缺运行批准、缺许可证、缺镜像或认证观察期未结束时进入等待，**保留"同目标唯一执行中操作"的槽位**，补齐后 `resume`；
- **同目标互斥**：同一租户 + 应用 + 环境同时只允许一个执行中操作（数据库唯一约束兜底）；
- **幂等**：相同幂等键 + 相同内容返回原操作；相同键、不同内容被拒；
- **租约**：执行者持租约推进；租约过期可被接管；**过期执行者不能覆盖新结果**；
- **结果未知**：阶段执行结果未知时不重复执行外部副作用，先核对证据/状态再决定。

### 10.3 权限

路由级只要求 `platform:application:read`；**动作级权限由处理器按服务端判定的动作校验**，缺一项即拒：

- 首次接入：`platform:application:create`、`platform:application-environment:create`、`platform:application-login-target:create`、`platform:oauth-client:create`、`platform:role-binding:update`；
- 接管 / 更新 / 恢复：`platform:application:update`、`platform:application-environment:update`、`platform:application-login-target:update`、`platform:oauth-client:disable`。

跨租户读取或恢复一律按"不存在"处理。

### 10.4 页面与 CLI

- 页面：「应用接入」页签内的**统一接入向导**（选择系统与环境 → 检查条件 → 确认操作 → 自动执行 → 查看结果／继续恢复）+ 进度组件（阶段结果分项、仍需处理条件与下一步）；旧接入模块同页保留；
- CLI：`scripts/subsystem.sh connect --application-code CODE --environment ENV`（`--resume <操作ID>` 继续、`--no-watch` 不轮询、`-y` 非交互）；CLI 与页面使用**同一组接口**，同一操作 ID、阶段与错误码；密码只经 `--password-stdin` 或受控 Cookie 文件，不作为命令行参数。

### 10.5 当前落地边界（务必据实理解）

1. **前置条件**：需先应用迁移 `000122`（操作/阶段表）。未应用时上述接口不可用，页面与 CLI 会返回错误；
2. **阶段执行器**：条件检查（CHECK）与目录登记（DIRECTORY）接既有服务；首次接入的认证（AUTH）由既有原子流程承担；
   受控更新（ADOPT/UPDATE/RETRY）的认证与部署**代码已实现**（观察期门禁 → 服务凭据含范围修复与轮换 → 浏览器凭据重建 → 更新装配 → Agent 更新），
   并有用例覆盖；**既有客户端缺失时仍由既有受控更新入口承担**（统一入口会明确失败并提示），且真实运行尚未验证（需先应用迁移 000122）；
3. **Worker**：持久执行器已接入平台 Worker 启停（`cfg.SubsystemOnboarding.Enabled` 为真时随 Worker 进程运行，周期领取并推进阶段）；
4. **就绪事实**：运行批准与商业授权为真实来源；镜像与认证观察期读取器未接线时，该项以**"未验证"**提示返回（既不当作就绪，也不当作缺失）；
5. **未完成业务验收前**，任何"业务可用"结论都不成立：结果区只做分项展示，不显示"业务可用"字样。
