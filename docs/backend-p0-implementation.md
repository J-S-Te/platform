# 基础能力平台 P0 后端实施蓝图

> 本文是当前 Vue 管理端的后端实施蓝图。`architecture.md`、`core-models-mysql.md`、`platform-integration.md` 仍是架构、数据库和跨系统接入的权威规范；本文不替代它们。

## 0. 当前实现状态（2026-07-16）

- 已完成后端优先级 1：MySQL P0 建表迁移、迁移版本记录/校验、并发迁移锁和平台初始数据。迁移入口为 `go run ./cmd/migrate` 或 `make migrate`。
- 已完成后端优先级 2：控制台密码登录、Ed25519 JWT HttpOnly Cookie、`iam_session` 持久化、会话刷新/退出、`/auth/me` 身份摘要，以及受保护请求的 JWT/会话/账号/租户一致性校验。认证路由位于 `/api/v1/auth`。
- 已完成后端优先级 3：用户、账号状态、组织单元、岗位和任职的 P0 HTTP/API 实现。用户和任职更新采用版本号乐观锁；手机号可选地使用 AES-256-GCM 加密保存，并仅返回脱敏值。当前公开 OpenAPI 未定义账号创建、密码初始化或重置接口，因此首个可登录管理员仍需后续受控初始化能力。
- RBAC 写接口与授权拦截、审计接收/查询/导出、配置 API 尚未实现；当前 `/auth/me` 仅读取现有 `USER` 角色绑定及其 `ALLOW` 权限摘要，不代表 RBAC 决策 API 已交付。
- 登录成功/失败和退出，以及本阶段用户、账号、组织、岗位和任职写操作的审计事件写入，将与后续审计模块一并实现；本阶段不会伪造已写入审计的结果。
- 初始数据只创建默认租户、内建平台应用、开发环境、根组织、P0 权限/角色和 `platform.console` 命名空间；不创建固定密码的管理员账号。

## 1. 目标与边界

- 架构：Go 模块化单体，Chi + `database/sql` + `sqlc`，MySQL；不使用 Redis、消息队列或微服务。
- P0：控制台密码登录和 JWT Cookie 会话、IAM、RBAC、授权检查、审计接收/查询/导出任务、配置命名空间与发布。
- P1：登录锁定/MFA/风险事件、审计归档、应用遥测元数据、告警、通知和字典；本期仅提供 API 契约。
- 不创建第二套业务用户、角色或审计模型；合同、项目、报销系统通过 API/SDK 接入，不直连平台表。

## 2. 工程结构与依赖

```text
cmd/{api,worker,migrate}/
internal/
  bootstrap/
  platform/{tenant,applicationregistry,identity,organization,authorization,security,audit,configuration,observability,notification,dictionary}/
  shared/{kernel,authctx,database,memorycache,messaging,observability,validation}/
  transport/http/
migrations/                 # 已实现 P0 建表/初始数据；后续仅新增 SQL 迁移
api/openapi/platform-p0.yaml
frontend/
docs/
```

每个领域模块均采用 `domain`、`application`、`infrastructure`、`interfaces/http` 四层。HTTP 层只做协议转换与参数校验；应用层定义事务和用例；领域层不依赖 Chi、SQL 驱动或文件系统；基础设施实现仓储和外部适配。

### 根目录 `.env` 约定

- 项目根目录的 `.env` 是本地开发和后续接入系统复用的统一运行配置入口；后端默认从该文件加载，亦可通过 `ENV_FILE` 指定其他路径。
- `.env.example` 是唯一允许提交的模板；`.env` 必须被 Git 忽略并仅限本地保存，不能写入真实密码、Token、私钥或生产配置。
- 系统环境变量优先级高于 `.env`，以便容器、CI/CD 和各接入系统安全覆盖配置。变量按 `APP_`、`MYSQL_`、`AUTH_`、`FILE_`、`LOG_`、`OTEL_` 等前缀划分，新增共享变量必须使用明确前缀。
- Vue/Vite 不会自动读取项目根目录 `.env`；前端仅在 `frontend/.env.local` 中使用非敏感 `VITE_*` 变量。任何认证密钥、数据库密码和服务端 Token 均不得使用 `VITE_*` 前缀。
- API 进程启动时必须提供匹配的 Ed25519 PKCS#8 私钥与 PKIX 公钥。开发环境可运行 `make generate-dev-jwt-keys` 生成 `data/keys/` 下的本地密钥，并在根目录 `.env` 配置对应路径；密钥目录不得提交。

请求链固定为：`Request ID → Recover → Access Log → CORS/Security Headers → Rate Limit → Authentication → Tenant Resolution → Authorization → Validation → Application Service → Audit`。中间件将 `Principal{tenant_id,user_id,account_id,session_id,org_ids,roles}` 写入 `context.Context`，业务代码不得自行读取认证 Header/Cookie。

## 3. 认证、会话与权限

### 3.1 控制台登录

- `POST /api/v1/auth/login` 仅接受 `login_type=password`、账号和密码；密码凭据采用 Argon2id 校验。
- 校验成功后创建 `iam_session`，返回 `Set-Cookie: bp_session=<JWT>`；JWT 至少携带 `sid`、`sub`、`tid`、`aid`、`iat`、`exp`、`iss`、`aud`。
- Cookie 必须为 `HttpOnly`、`Secure`（开发环境可配置关闭）、`SameSite=Lax`、`Path=/`；浏览器端不保存 access/refresh token。
- 每个受保护请求都需校验 JWT、会话状态、过期时间、账号状态和租户一致性。刷新、退出和会话撤销必须同步更新 `iam_session`。
- OIDC/OAuth 对外端点为 P1 预留，不能伪装为当前控制台密码登录的实现。

### 3.2 权限原则

- 权限编码为 `{application}:{resource}:{action}`；菜单可见性不等于后端授权。
- 所有写操作按资源权限执行，并记录审计事件；高风险操作按 P1 Step-up 策略扩展。
- `authz_policy_revision` 变更后更新进程内权限缓存版本；不使用 Redis。

## 4. P0 数据模型映射

| 能力 | P0 主表 | 后端职责 |
|---|---|---|
| 租户与应用 | `iam_tenant`、`platform_application`、`platform_application_environment` | 默认租户解析、应用/环境隔离 |
| 登录身份 | `iam_user`、`iam_account`、`iam_password_credential`、`iam_session` | 账号认证、禁用与会话撤销 |
| 组织任职 | `iam_org_unit`、`iam_position`、`iam_membership` | 主组织、兼岗、历史任职 |
| 授权 | `authz_resource`、`authz_permission`、`authz_role`、`authz_role_permission`、`authz_role_binding`、`authz_policy_revision` | 角色绑定和决策 |
| 审计 | `audit_event_dedup`、`audit_event` | 幂等接收、查询、受控导出 |
| 配置 | `cfg_namespace`、`cfg_item`、`cfg_release`、`cfg_release_item` | 命名空间、版本发布与读取 |
| 异步任务 | `async_job` | 导出、归档、通知等可重试后台任务 |

所有聚合根使用 ULID、`tenant_id`、`status`、`version`、UTC `DATETIME(3)` 和创建/更新人字段。审计事件可保留内部 `subject`、`request_id`、`trace_id`，但当前控制台列表、详情和导出 DTO 不对外返回这三类字段。

## 5. 前端页面对接

| 前端区域 | API 分组 | 实施状态 |
|---|---|---|
| 登录页 | `auth` | 已实现密码登录、刷新、退出、当前身份；首个管理员创建仍待 IAM 用户/账号优先级 |
| 用户、账号、组织任职 | `identity`、`organization` | 已实现用户、账号查询/状态更新、组织、岗位、任职 API；账号创建/密码初始化仍待补充 OpenAPI 契约 |
| 角色、角色绑定、权限注册 | `authorization` | 待实现（后续优先级） |
| 审计日志 | `audit` | 待实现（后续优先级）；不支持删除 |
| 基础设置 | `configuration` | 数据模型与 `platform.console` 命名空间已初始化；API 待实现 |
| 通知设置、字典 | `notification`、`dictionary` | P1 契约；仅站内信和邮件，不含短信 |
| 登录安全、风险事件 | `security` | P1 契约 |
| 审计上报状态、告警、遥测 | `audit`、`observability` | P1 契约；日志/Trace/Metric 不写 MySQL |

## 6. 审计与异步原则

- 平台自身的登录、用户/角色/权限/配置写操作必须写入 `audit_event`。
- 接入系统先在本地事务写 `audit_outbox`，再调用平台单条或批量审计接收接口；平台按 `event_id` 幂等。
- 审计事件不可通过业务 API 删除。导出创建异步任务，归档/保留由受控 Worker 执行并额外审计。
- “审计上报重试”仅重放平台接收侧死信或失败任务，不得越权操作外部业务系统的本地 Outbox。

## 7. 实施验收

- 所有 API 位于 `/api/v1` 并实现 OpenAPI 文档中的成功/失败响应。
- 已实现的用户、账号和任职更新验证 `version`、租户并在冲突时返回 `IAM_VERSION_CONFLICT`；权限强制与审计写入将在后续模块接入。
- 所有列表遵循 `page`、`page_size`、`keyword`、`filter[...]` 格式。
- 没有 Redis 依赖；文件仅保存到 `FILE_STORAGE_ROOT`；实际运行日志与 OTel 数据不进入业务 MySQL。
