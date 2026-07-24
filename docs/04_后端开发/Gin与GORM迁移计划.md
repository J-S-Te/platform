# Gin 与 GORM 技术迁移计划

## 1. 目的与范围

本项目的正式后端技术基线为 **Gin + GORM + MySQL**。迁移、认证与 IAM 管理能力此前使用 Chi、`net/http` 和 `database/sql` 实现；本计划中的 M0～M4 已于 **2026-07-19** 完成代码迁移与静态准入验证。后续 RBAC、审计、配置等模块必须沿用同一套 Web 与持久化范式。

本计划只迁移技术实现方式，不改变既有 OpenAPI 路径、JSON 字段、Cookie 名称、JWT Claims、MySQL 表结构、迁移版本和业务规则。迁移过程中不得创建第二套表、第二套身份模型或兼容性 API。

## 2. 强制技术决策

| 范围 | 统一方案 | 禁止事项 |
|---|---|---|
| HTTP Web 框架 | Gin，使用 `gin.New()` 显式装配中间件 | Chi、新增裸 `net/http` 路由、`gin.Default()` 隐式中间件 |
| 运行时数据访问 | GORM + MySQL Driver | 应用仓储直接使用 `database/sql`、sqlc、新增手写 `Query`/`Exec`/`Scan` |
| 事务 | application 服务定义原子用例边界；复合持久化仓储方法以 GORM `db.Transaction` 落地同一用例的多条 SQL | Handler 开启事务、与既定复合用例无关的事务 |
| Schema 演进 | `migrations/` 中版本化 SQL，执行时可由 GORM `Exec` | `AutoMigrate`、修改已执行迁移文件 |
| 领域模型 | 纯 Go，基础设施层行模型单独映射 | 在 domain 中嵌入 `gorm.Model` 或暴露 GORM Tag |
| 多租户与并发 | 每次查询显式 `tenant_id`，更新含 `version` | 仅按 `id` 更新租户数据、忽略 `RowsAffected` |

## 3. 迁移前不变性清单

以下内容必须保持不变，并通过迁移前后的契约测试验证：

- `/api/v1` 路径、HTTP 方法、状态码和 OpenAPI DTO。
- 错误响应 `{ code, message, request_id, details }` 的结构与稳定错误码。
- `bp_session` HttpOnly JWT Cookie 的属性、Claims、续期与退出语义。
- Argon2id 密码验证、Ed25519 JWT 签名、手机号 AES-256-GCM 加密/脱敏规则。
- ULID、UTC `DATETIME(3)`、`tenant_id` 隔离、乐观锁 `version` 以及现有迁移文件的 SHA-256 校验。

## 4. 实施顺序

### 4.1 阶段 M0：依赖与启动装配（已完成，2026-07-19）

1. 在 `go.mod` 增加 `github.com/gin-gonic/gin`、`gorm.io/gorm` 和 `gorm.io/driver/mysql`。
2. 在 `internal/shared/database` 建立 GORM 连接工厂，复用现有 `.env` 中的 MySQL 配置；连接池参数、时区、超时和 TLS 配置必须保持原有语义。
3. 启用 GORM 错误翻译；数据库连通性由 `/readyz` 通过 GORM 健康检查确认，保持 API 进程可在 MySQL 暂不可用时启动并提供存活探针。
4. 不新增数据库连接配置，不修改根目录 `.env.example` 中既有 MySQL 变量名称；GORM 是访问库替换，不是新数据库。

**验收：** API 与迁移命令可使用 GORM 初始化连接；缺失或格式错误的 MySQL 配置仍会在启动阶段失败并给出不泄露凭据的错误，数据库不可达时 `/readyz` 返回不可用。

### 4.2 阶段 M1：迁移执行器（已完成，2026-07-19）

1. 保留 `migrations/` 文件、顺序编号、checksum 和 `platform_schema_migration` 语义。
2. 将迁移元数据查询、`GET_LOCK`/`RELEASE_LOCK` 和 SQL 文件执行迁移为 GORM `WithContext`、`Raw`、`Exec`。
3. MySQL DDL 的隐式提交特性不变：每条语句独立执行，失败后由现有 checksum/版本机制阻止错误重放。
4. 禁止以 `AutoMigrate`、`Migrator` 自动比对或代码生成代替已有 SQL 迁移。

**验收：** 全新空库、已迁移数据库和 checksum 被篡改数据库三类场景均保留原有结果。

### 4.3 阶段 M2：Gin 路由与中间件（已完成，2026-07-19）

1. 将 Router 从 Chi 替换为 `*gin.Engine`，保留健康检查、`/api/v1/auth/*` 与 IAM 路由的路径和方法。
2. 按顺序实现 Request ID、Recover、结构化访问日志、CORS/安全头、认证、租户解析和授权中间件。使用 `gin.New()`，不依赖 `gin.Default()`。
3. 将 `Principal`、Request ID 和 Trace ID 写入 `c.Request.Context()`；Gin Key 仅作 Handler 内部辅助访问。
4. 统一严格 JSON 解码、分页参数、错误响应、Cookie 和导出流响应；路由参数通过 Gin 读取，禁止让 Gin 默认错误格式泄露到 API。
5. 将 Handler 测试从 `net/http` 路由构造改为 Gin 测试 Engine，并加入未知 JSON 字段、认证失败、请求 ID 和错误响应兼容性测试。

**验收：** OpenAPI 契约测试和现有登录/IAM Handler 测试在 Gin 上通过，前端无需调整请求代码。

### 4.4 阶段 M3：GORM 持久化模型与仓储（已完成，2026-07-19）

按模块逐个迁移，顺序固定为：

1. `identity`：账号、密码凭据、会话、用户。
2. `organization`：组织单元、岗位、任职。
3. 已实现的 IAM 查询/更新用例。
4. 后续 `authorization`、`audit`、`configuration` 模块只允许以 GORM 新增。

每个模块迁移时必须：

- 在 `infrastructure` 包或其 `persistence` 子包中创建独立的 GORM 行模型和 mapper。
- 使用 `db.WithContext(ctx)`；所有租户模型使用显式 `tenant_id` 条件。
- 对乐观锁更新使用 `WHERE id AND tenant_id AND version`，根据 `RowsAffected` 处理版本冲突。
- 对需要原子性的一组写操作，由 application 服务以单个复合持久化用例确定边界；仓储在该用例内通过 `db.Transaction` 使用同一个 `*gorm.DB` 执行全部 SQL。
- 对唯一键、外键、记录不存在、死锁、超时和重复提交统一映射平台错误码。

**验收：** 每个仓储同时覆盖成功、租户隔离、版本冲突、唯一键冲突和回滚场景；不再残留应用层直接 `database/sql` 访问。

### 4.5 阶段 M4：清理与准入（代码与静态准入已完成，真实 MySQL 验收待隔离环境）

1. 删除 Chi 路由注册和已被 GORM 替代的 `database/sql` 仓储实现。
2. 从直接依赖中移除 Chi、sqlc 及仅服务于旧仓储的辅助代码；`go-sql-driver/mysql` 可能作为 GORM MySQL Driver 的间接依赖保留，无需强行删除。
3. 执行 `go test ./...`、`go vet ./...`、迁移测试、Gin 路由契约测试和真实 MySQL 集成测试。
4. 代码评审检查：新文件不得引入 Chi、`database/sql` 仓储调用、`AutoMigrate` 或领域模型 GORM Tag。

完成 M4 前，**不得开始优先级 4（RBAC 写接口与授权拦截）**，以避免后续模块建立在已废弃技术栈上。

## 5. GORM 实现约束

### 5.1 模型与字段

- 行模型显式映射表名、主键、`DATETIME(3)`、`version`、审计字段和 JSON 列；使用 `TableName()` 固定表名。
- 不使用 `gorm.Model`、默认软删除、自动关联保存或全字段 `Save`。
- 对外输入转领域命令后，仓储通过字段白名单 `Select`/`Omit` 写入；不得让 Request DTO 直接执行 `Updates`。

### 5.2 查询、锁与事务

- 所有查询使用参数绑定；只有分区 DDL、MySQL 锁和经评审的复杂报表可使用 `Raw`，并仍要通过 GORM 和上下文执行。
- 读取关系数据时显式选择列并限制 `Preload`；禁止 `Preload(clause.Associations)`。
- 默认关闭 GORM 隐式单写事务；application 用例必须明确原子操作范围，仓储仅为既定的复合写用例开启 `db.Transaction`。任何涉及会话撤销、成员关系切换、角色绑定、审计去重或配置发布的操作必须在一个显式事务中完成。
- 日志只能记录错误类别、表/操作和 Request ID；不得记录密码、JWT、手机号明文、完整 SQL 参数或敏感 JSON。

## 6. 当前状态与后续开发准入

截至 **2026-07-19**，优先级 1（迁移）、2（认证会话）、3（IAM 用户、组织与任职）、4（RBAC 写接口与授权拦截）、5（审计安全与可观测性 P0 基线）、6（配置中心 P0 基线）、7（外部应用凭据、审计导出与身份生命周期审计）、8（登录安全与风险控制）和 9（平台设置、通知设置与字典管理）均已按 Gin + GORM 落地。HTTP 入口使用 `gin.New()` 显式中间件链；迁移执行器和全部已实现的领域仓储均通过 GORM 访问 MySQL；版本化 SQL 迁移仍是唯一的 schema 演进方式。

M4 已完成以下代码与静态准入验证：

- 已执行 `go test ./...` 和 `go vet ./...`，所有当前 Go 包通过。
- 已执行 `git diff --check`、Makefile 干运行，以及对 Go 源码的依赖扫描；未发现 Chi、应用仓储 `database/sql` 或自动 schema 演进 API 残留。
- 已新增根目录 `.env.example` 作为前后端及后续接入系统复用的环境变量模板；后端从 `backend/` 启动时会回退加载项目根目录 `.env`，相对路径以实际加载的环境文件所在目录为基准。

真实 MySQL 集成测试仍应在隔离的验收数据库中执行：覆盖空库迁移、已迁移库幂等执行、checksum 篡改、登录会话、租户隔离、乐观锁冲突与事务回滚。该验证不会默认连接或修改开发者本地 `.env` 指向的数据库。

优先级 4、5、6、7、8、9 已在当前代码库完成既定范围实现；OIDC/OAuth P1 的基础协议运行时也已补齐。P7 已提供 Application/Environment/OAuth Client 受控管理、Scope 与精确回调地址维护、客户端密钥创建/禁用/轮换、`client_secret_basic` + `client_credentials`、`audit.ingest` 上报边界、MySQL 异步导出 Worker 与本地 CSV 下载。OIDC 运行时已提供 `GET /authorize`、Authorization Code、PKCE（`S256`/`plain`）、Refresh Token 轮换与复用检测、RFC 7009 撤销、Discovery、JWKS、UserInfo 和 Logout / Post Logout Redirect，协议状态通过版本化 SQL 迁移持久化到 MySQL，令牌明文不入库。P8 已提供登录策略、连续失败锁定、管理员解锁、风险事件查询与处置，以及登录入口进程内固定窗口限流。P9 已提供平台资料、历史兼容通知偏好和业务字典/字典项管理；后续迁移已补齐独立站内信模板、消息投递、收件箱与失败重试，不提供邮件、短信、SMTP 或 Webhook 发送。MFA、标准 OIDC 外部身份登录、钉钉预绑定扫码登录、用户授权同意后端接口、`private_key_jwt`、PAR/JAR 和独立 post-logout URI 注册已在后续迭代落地。后续应优先补齐独立 Vue 同意页、Telemetry/Metric/Trace、告警、审计归档和长期保留。任何后续模块仍必须遵守本计划的 Gin、GORM、显式 SQL 迁移、租户隔离与禁止 `AutoMigrate` 约束。

## 7. 本次完成记录（2026-07-19）

- 已移除 Chi 路由和旧的 `database/sql` 仓储实现；HTTP 入口改为 `gin.New()` 显式中间件链，迁移元数据、迁移执行和身份组织持久化统一通过 GORM。
- 已保留既有版本化 SQL 迁移、MySQL advisory lock、`tenant_id` 过滤、`version` 乐观锁、Cookie 与 API 响应契约；未使用 GORM 自动 schema 演进。
- 已新增 GORM 表名映射和根目录环境文件回退的单元测试，并完成当前 Go 测试与静态检查。
- 已完成迁移后的前后端基础安全复检：登录成功回跳仅允许当前站点地址，Gin 安全中间件补充默认拒绝策略的 Content-Security-Policy，且前端不存储会话 JWT。



## 8. P9 设置、通知与字典完成记录（2026-07-19）

- 已新增版本化 SQL 迁移 `000015_create_settings_and_dictionary.sql`，创建 `platform_setting`、`notification_setting`、`dict_dictionary`、`dict_item` 及对应的资源、权限和内建角色授权；未使用 `AutoMigrate`。
- `settings` 与 `dictionary` 均遵循 `domain`、`application`、`infrastructure`、`interfaces/http` 分层；领域模型不包含 GORM Tag，GORM 行模型与领域模型分离。仓储使用显式事务、`tenant_id` 范围和 `version` 乐观锁。
- Gin 路由已声明平台设置、通知设置、字典和字典项的读写权限；平台设置与通知设置首次读取返回默认值，后续写入生成或更新租户唯一记录。
- 历史通知偏好表仍保存 `inbox_enabled`、`email_enabled`、`reminder_frequency` 以保持数据库与接口兼容，其中 `email_enabled` 不会启用邮件发送。实际站内信使用独立模板、消息和投递模型；平台不提供短信、邮件、SMTP 或 Webhook 发送。字典运行时读取只返回启用父字典下的启用项，并按 `sort_order` 排序。
- 已补充领域服务单元测试、迁移内容测试和 OpenAPI 契约；真实 MySQL 集成验证仍应使用隔离验收数据库执行。


## 8. P4～P8 迁移完成记录（2026-07-19）

- `authorization`、`audit`、`configuration` 均遵循 `domain`、`application`、`infrastructure`、`interfaces/http` 分层；GORM 行模型与领域模型分离，未在领域模型中引入 GORM Tag。
- RBAC、审计事件幂等/导出任务、配置发布均在 application 用例定义的显式 GORM 事务边界内执行；配置发布使用命名空间锁与版本校验，审计接收使用事件去重。
- Gin 路由已为全部 P0 管理接口声明权限；审计中间件只记录安全摘要，敏感字段会脱敏且不采集请求或响应正文。审计查询限流使用进程内固定窗口，不依赖 Redis。
- P7 已实现 Application、Environment、OAuth Client、Scope、精确回调地址和客户端密钥创建/禁用/轮换的受控管理接口；所有写操作经 RBAC 路由保护并进入审计链。外部应用运行时已实现 `client_secret_basic` + `client_credentials` Bearer 认证、`audit.ingest` 上报边界、MySQL `async_job` CSV 导出 Worker/租户权限下载，以及成功登录/退出的最佳努力审计。客户端密钥明文只在创建或轮换响应中显示一次，散列与指纹持久化，轮换重叠窗口最长 30 天。
- P8 已实现基于 MySQL 的登录策略与失败尝试记录、账号锁定、管理员解锁、风险事件查询与处置；认证失败审计仅关联已识别账号。后续迭代已补齐 TOTP MFA、标准 OIDC 外部身份登录、钉钉预绑定扫码登录、用户同意后端接口、`private_key_jwt`、PAR/JAR 与独立 post-logout URI 注册。独立 Vue 同意页、审计归档与长期保留仍未完成，不能以“已完成”名义对外承诺。
