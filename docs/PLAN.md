# 基础能力平台后端结构模型与文档蓝图

## 总结
基于现有 Vue 前端和 `docs/` 既有规范，新增面向实施的后端蓝图与 OpenAPI 3.1 草案；**不创建 Go 代码、MySQL 迁移脚本或修改现有前端**。既有 `docs/architecture.md`、`docs/core-models-mysql.md`、`docs/platform-integration.md` 保持为架构与模型的权威来源。

## 文档交付
- 新增 `docs/backend-p0-implementation.md`：定义 Go 模块化单体目录、模块边界、依赖方向、请求中间件链、Chi + `database/sql` + `sqlc` 的实现基线、事务/异步任务边界，以及与当前前端页面的能力映射。
- 新增 `docs/backend-model-mapping.md`：将前端的用户、账号、组织任职、角色、角色绑定、权限、审计、平台设置、安全与可观测页面，映射到 MySQL P0 表、P1 预留模型、状态码、权限点和数据脱敏规则。
- 新增 `docs/frontend-backend-contract.md`：定义前端从 mock 数据切换到 API 的调用约定、分页/筛选格式、统一错误响应、Cookie 会话行为，以及未实现 P1 页面如何返回能力状态。
- 新增 `api/openapi/platform-p0.yaml`：提供 OpenAPI 3.1 草案，覆盖 P0 可实现接口与 P1 前端契约接口。

## 后端模型与接口决策
- 采用模块化单体：`tenant`、`applicationregistry`、`identity`、`organization`、`authorization`、`security`、`audit`、`configuration`、`observability`、`notification`、`dictionary`、`shared`、`transport/http`；模块仅通过公开 application contract 或应用内事件协作。
- 持久化严格遵循现有 MySQL 规范：ULID、UTC `DATETIME(3)`、`tenant_id` 隔离、聚合根 `version` 乐观锁、稳定英文状态码、审计分区表无外键；不使用 Redis，文件仍为本地受控目录方案。
- P0 落模型：应用/环境/客户端、租户、用户、账号、密码凭据、组织/岗位/任职、会话、资源/权限/角色/角色权限/角色绑定/策略版本、审计去重/审计事件、配置命名空间/条目/发布、文件元数据与异步任务。
- 控制台认证仅实现当前前端契约：`POST /api/v1/auth/login` 校验账号密码后签发 **JWT Cookie**；JWT 包含会话、租户、用户、账号和过期信息，`iam_session` 保存会话状态、撤销状态与到期时间。Cookie 固定为 `HttpOnly`、`Secure`、`SameSite=Lax`，前端仅以 `credentials: include` 调用接口，不保存 token。
- OIDC/OAuth、外部身份、MFA、风险事件、审计归档、告警规则、可观测性配置属于 P1：在模型映射与 OpenAPI 中保留契约，不在本期定义为可运行能力。
- 审计事件后端仍按既有规范保留 `subject`、`request_id`、`trace_id` 等内部字段；当前前端列表、详情和导出 DTO 不返回这些已移除字段。审计只提供查询、详情和导出，不提供直接删除接口；归档/清理由受控保留任务完成并产生新的审计事件。
- 当前“审计上报重试”页面只定义平台接收侧状态和平台死信重放能力，不允许平台直接操作业务系统本地 `audit_outbox`。
- 运行日志、Trace、Metric 不写入 MySQL；仅为 `obs_service`、日志策略和告警规则保留 P1 配置模型与查询入口契约。通知设置仅规划站内信和邮件，不恢复短信能力；字典管理保持 P1 预留。

## OpenAPI 覆盖范围
- P0：登录/刷新/退出/当前会话；用户、账号、组织、岗位、任职；资源、权限、角色、角色绑定；授权单次与批量判定；审计事件接收、批量接收、分页查询、详情与导出任务；配置命名空间、条目、发布与读取。
- 前端契约预留：基础设置、邮件与站内信通知设置、登录安全策略、锁定账户、风险事件、审计接收状态、服务遥测元数据、告警规则与字典管理。
- 统一采用 `/api/v1`、`page`/`page_size`、`keyword`、`filter[...]`、稳定错误码与 `{ code, message, request_id, details }` 错误响应格式；写操作明确要求权限点、审计事件和乐观锁版本。

## 验证标准
- 新文档中的表、模块、API 与现有 `docs/` P0/P1 分层无冲突。
- 每个现有前端页面/操作均能在前后端契约文档中找到对应 API、权限、模型归属或明确的 P1 占位状态。
- OpenAPI 3.1 文件可通过语法校验；所有 P0 写接口声明鉴权、租户上下文、错误响应和审计要求。
- 审计接口中不存在直接删除事件的端点；前端已隐藏的主体、Request ID、Trace ID 不出现在列表、详情和导出响应 DTO 中。

## 默认假设
- 本次只交付模型和文档，不初始化 `go.mod`、不写 Go 代码、不写 SQL 文件、不修改 Vue 页面。
- 后续真正实现时采用 Go、Chi、`database/sql`、`sqlc`、MySQL 和 JWT Cookie 会话；密码凭据使用 Argon2id 存储。
- 当前仅存在默认租户，但所有模型和 API 从第一版保留租户上下文与隔离字段。
