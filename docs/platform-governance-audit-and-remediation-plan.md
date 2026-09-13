# 平台治理能力审计与整改方案

> 日期：2026-09-12
> 性质：**审计 + 整改方案**（本文不含代码改动）
> 审计方式：**跨 7 个仓库的静态代码审计**（platform / contract_management / customer_and_opportunity / project_management / Settlement / data_analysis / frontend）。
> **证据等级声明**：本文结论均来自源码、迁移 SQL 与配置的静态证据，**未做双进程并发实测、未跑集成测试、未检查线上数据**。凡属推断的结论已单独标注。生产环境实际生效的环境变量（如 `APP_TRUSTED_PROXIES`、各子系统通知/审计凭据）在仓库中不可见，需部署侧确认。

---

## 1. 总体结论

| # | 需求 | 结论 | 一句话 |
| --- | --- | --- | --- |
| ① | 平台做所有子系统登录管控与权限赋权，子系统仅作业务 | **基本实现，有 3 个越权/旁路缺口** | 登录确实全部收敛到平台 OIDC；但目录推送有 2 个子系统未真正做到，另有硬编码 admin 旁路与行级范围降级 |
| ② | 每个子系统的业务操作 + 跨系统站内通知 | **部分实现，是差距最大的一项** | 平台框架完整，但 2 个子系统实际零通知、1 个必然失效、跨系统提醒实为"投到平台收件箱"，且普通用户看不到自己的收件箱 |
| ③ | 所有系统日志审计均由平台集中记录 | **部分实现** | 平台侧能力完整且设计良好；但 4 个子系统是 fire-and-forget，失败登录大面积缺失，且存在可记录成 `127.0.0.1` 的配置陷阱 |
| ④ | 子系统登录地址是外网 IP 而非 127 开头 | **部分实现** | IP 解析机制正确（伪造 XFF 会被忽略），但**无 fail-closed 校验**，配置漏配即静默记录 `127.0.0.1`/内网 IP；登录落地地址校验也接受 loopback |
| ⑤ | 各子系统业务流已实际点击跑通、无报错 | **无法验证，且该验收方式已被证明存在盲区** | 见 §7：project_management 的通知链路 100% 不可用，而"点按钮无报错"完全成立 |
| ⑥ | 多人同时修改同一对象的并发处理 | **部分实现，各仓库差距极大** | 2 个仓库有完整防护，2 个有真缺口，2 个几乎为零；且前端有 2 个模块完全不回传版本、不处理 409 |

**一句话总览**：**平台侧的"底座"（OIDC、授权中心、审计、通知框架）做得相当完整；问题几乎全部集中在"子系统侧的落实"和"跨系统契约的统一"上。** 最严重的三处是：project_management 通知必然失效、Settlement 目录从未推送到平台、Settlement 认证路径完全无审计。

---

## 2. 需求①：登录管控与权限赋权

### 2.1 已实现（正面证据）

| 项 | 证据 |
| --- | --- |
| 登录全部收敛到平台 | 各子系统 `/auth/login` 均为 OIDC flow 跳转（contract `router.go:96`、project `api.go:81`、Settlement `api.go:76`），非本地口令校验 |
| 子系统无本地账号体系 | 5 个仓库迁移中 `password` 列命中数均为 **0**；全仓无 `bcrypt`/`argon2`/`CompareHashAndPassword` |
| 权限判定用平台签发权限码 | `platform/internal/transport/http/middleware/authorization.go:14-31`，基于服务端会话 `principal.PermissionCodes`，不信任请求头 |
| 目录推送接收端完备 | `PUT /api/v1/applications/:id/authorization-catalog`（`router.go:546`），校验应用归属 + scope `authorization.catalog.sync`，落库并做版本+摘要幂等 |
| 会话吊销真实生效 | backchannel logout outbox `platform_oauth_backchannel_logout_outbox` + 独立 dispatcher |
| 授权上下文设计合理 | 在线短缓存（非写进 token），含 roles/permissions/data_scopes |

### 2.2 问题

**P0-①-A｜Settlement 的角色权限目录从未推送到平台**
- 证据：`Settlement` 只有 `authz/permission-manifest.json`（12 权限/4 角色，内容完整）与 `authz/embed.go`；**全仓无任何 `authorization-catalog` 推送代码**，`cmd/` 下也没有 authz-catalog 命令；平台迁移里也没有 settlement seed。
- 影响：Settlement 的角色与权限码不在平台授权中心，无法按"推送到平台做分配认证"的方式赋权，只能人工在平台控制台逐条建。**直接违背需求①**。
- 整改：新增 `cmd/authz-catalog`（照抄 contract `internal/infrastructure/platform/catalog.go` 的实现），接入 `subsystem.sh`/provisioner 的目录发布流程，并纳入清单的 `gateway`/发布步骤。

**P0-①-B｜data_analysis 的目录推送是"假入口 + 内容漂移"**
- 证据：`data_analysis/cmd/authz-catalog/main.go` **全文 14 行，函数体只有一条 `log.Printf("占位…")` 与 `// TODO`**，不构建目录、不发请求；真实推送发生在 `cmd/dashboard-api/main.go:40` 启动期。且目录内容硬编码在 `internal/platformcatalog/manifests.go:5-39`，与 `authz/permission-manifest.yaml` **已经漂移**（Go 版多出 admin 角色），两者之间无机器校验。
- 影响：①运维按惯例执行 `authz-catalog publish` 会静默成功但不产生任何效果；②平台侧目录与仓库声明不一致，且无人能发现。
- 整改：把 `cmd/authz-catalog` 落为真实实现（单一数据源读 `authz/permission-manifest.yaml`），删除 Go 内硬编码副本；在 CI 加"目录内容与 manifest 一致性"校验测试。

**P0-①-C｜两个越权缺口**
1. **contract_management 硬编码 admin 旁路**：`internal/application/templates.go:209-211` `hasPermissionOrAdmin = actor.Has(perm) || hasRole(actor,"admin")`。持有 `admin` 角色即绕过权限码校验——与"权限由平台目录驱动"冲突（若平台收窄 admin 权限，此处仍放行）。
2. **data_analysis 行级数据范围降级**：`internal/embedbridge/embed.go:76` 对所有角色**硬编码** `scope_mode: "TENANT"`，而 manifest 声明 `sales_director=ORG_SUBTREE`、`tech_director=TEAM`。结果这两个角色实际拿到**全租户**数据。
- 整改：①删除 admin 旁路，改为在平台目录里给 admin 角色显式授予该权限；②行级范围必须取自授权上下文（`data_scopes`），不得在子系统内硬编码。

**P1-①-D｜平台单点与降级策略未定义**
- 证据：授权上下文是在线短缓存（子系统定期刷新，如 project 的 `OIDC_AUTHORIZATION_REFRESH_INTERVAL`）。
- 风险（**属推断，需实测**）：平台不可用时，子系统是"沿用最后已知权限（fail-open）"还是"拒绝服务（fail-closed）"？两者安全含义相反，当前无明确设计与文档。
- 整改：明确并文档化降级语义（建议：授权刷新失败时沿用最后已知权限并降级告警，但**不延长**会话有效期），并加入演练。

---

## 3. 需求②：站内通知

### 3.1 已实现（平台侧框架完整）

表：`notification_template` / `_version` / `notification_message` / `notification_delivery`（`000022`）、`notification_event_inbox` / `notification_user_stat`（`000093`）。
统一跨系统入口：`POST /api/v1/notifications/events`（+`/batch`、`/receipts/:id`），scope `notification.ingest`；受众支持 USER/ROLE/ORGANIZATION；已读与未读统计同事务；幂等键齐备；**摄取租户取自机器凭据而非请求体**（`handler.go:153-156`），隔离正确。
**未发现任何子系统直写平台库**（5 个仓库各只有自有 DSN），全部走 HTTP —— 这一点做得好。

### 3.2 问题

**P0-②-A｜project_management 的通知链路"代码写了但必然不生效"**
- 证据：`delivery.go:1243` 把 scope 写死为 `"application"`，而平台白名单只接受 `CROSS_SYSTEM|PLATFORM`（`platform/.../notification/application/ingestion.go:143`）→ **必然 400**；endpoint 回退到 `/internal/v1/notifications/events`（`notification.go:16`）与真实路由 `/api/v1/notifications/events` 不符 → **必然 404**；无本地表、无 outbox、无投递 worker；失败仅 `Warn`；`NOTIFICATION_ENABLED` 默认 `false` 且部署未配凭据 → publisher 为 nil。
- 影响：项目子系统**所有**业务操作（立项、指派、偏差上报/评审、超期）均无站内提醒；跨系统提醒为 0。
- 整改：scope 改 `CROSS_SYSTEM`；endpoint 统一；新建 `pm_notification_outbox` + worker（照抄 contract/Settlement 模式）把通知移出请求路径；补事件与收件人（被指派人 `team_lead_id`/`project_manager_id`/`engineer_ids` 目前从不作为收件人）。

**P0-②-B｜data_analysis 零通知实现**
- 证据：21 张迁移表无通知/outbox 表；全仓无 `/notifications/events` 调用；`alert_rule_recipient`（`000011:32-42`）已建但 **Go 代码零引用 = 死表**。
- 影响：告警是本系统核心产出，"触发即失联"。
- 整改：建 `da_notification_outbox` + worker；告警 upsert 事务内写 outbox；收件人取 `alert_rule_recipient`。

**P0-②-C｜Settlement 只有 1 个通知事件，且审批结果不通知申请人**
- 证据：唯一通知是催收逾期（`dunning/scanner.go:51-91`）；`operations.go:179-186` **已经把 `applicant` 查出来了却没有使用**。
- 影响：开票申请人无法感知审批结果，只能轮询。
- 整改：用现成的 `outbox(ctx, tx, ...)` 在审批/开票/收款/核销/冲销/计划确认处补 `PLATFORM_NOTIFICATION`（同事务）。

**P1-②-D｜平台投递失败无自动重试，摄取无限重试且无告警**
- 证据：`service.go:174-198` 的投递重试**无任何调用方**（仅手工 `POST /notifications/deliveries/retry`），`idx_notification_delivery_retry` 无消费者 → FAILED 永久滞留；摄取失败固定 1 分钟重试、无 attempt 上限/退避（`ingestion.go:83`），DEAD 仅落表且告警模块已在 `000040` 移除。
- 整改：加 delivery retry worker（复用 `worker.go:163` 的注册模式）；摄取加 attempt 上限 + 指数退避 + DEAD 可见告警。

**P1-②-E｜普通业务用户看不到自己的站内通知（与需求目标直接冲突）**
- 证据：前端 notify tab 的权限是 `platform:notification-setting:read/update`（管理类权限，`platformConsoleAccess.js:21-23`），入口还放在"设置"下；后端 inbox 接口本身**无权限门**（`router.go:455-459`）。且前端通知中心仅 `onMounted` 拉取，**全仓无 EventSource/WebSocket**，平台顶栏铃铛是硬编码 toast（无未读角标）。
- 整改：收件箱提升为全局入口（仅需登录，无需管理权限）；补未读角标 + 轮询（或 SSE）。

**P1-②-F｜契约不统一 + CRM 队头饥饿**
- 契约：4 个子系统用了 **3 个 endpoint、2 种 scope**（contract/CRM `/api/v1/notifications/events`、Settlement `/batch`、PM `/internal/v1/...`）。
- CRM 队头饥饿：`notificationdeliveryworker/worker.go:81` 每次取 `platform_delivered_at IS NULL AND platform_delivery_failed_at IS NULL ORDER BY id ASC LIMIT N`，失败仅 `logger.Error + continue` 不回写重试计数/退避 → 某条被平台持续 5xx 的事件永久占队头，**堆积到 BatchSize 后其后通知永不投递**。
- 整改：在 sharedDocs 固化唯一契约；CRM 补 `delivery_attempts/next_attempt_at/last_error` 或改用 outbox 表。

**P2-②-G｜通知体系三套并行 + 幂等键失效 + 错误被吞**
- CRM 自建收件箱 + 平台收件箱双写（同一消息可能两处展示）；contract/project 的"通知"实为本地待办推算；Settlement 铃铛静态无数据。
- 平台生命周期通知幂等键用 `time.Now().UnixNano()`（`operational_modules.go:307`）→ 重试产生重复通知。
- 人员异动通知错误被吞（`personnel_change.go:198` `_, _ = ...`）。
- `email_enabled` 是空配置（无任何发送实现，前端也无设置 UI）。

**P2-②-H｜"跨系统通知"名不副实**
- 证据：所谓跨系统通知实际只是"投递到平台中央收件箱"，收件人全部解析为平台内部用户；contract→项目只有业务事件投递（非通知）。
- 缺失样例：结算逾期提醒合同负责人、合同签署完成提醒项目经理 —— **均未实现**。
- 整改：定义跨系统提醒矩阵（事件 → 目标角色码），复用平台 ingestion 的 role 解析。

---

## 4. 需求③：集中审计

### 4.1 已实现

平台侧能力完整：`audit_event` 表（`000007`）+ 摄取端点 `POST /api/v1/audit/events[/batch]`（`router.go:560-561`，scope `audit.ingest`，独立的 bearer 边界）+ 查询 `GET /audit/events`（`platform:audit:view`）+ 导出作业（`platform:audit:export`）+ 登录策略/尝试/风险事件表（`sec_login_policy`/`sec_login_attempt`/`sec_risk_event`）。字段覆盖操作者/租户/应用/环境/客户端 IP/UA/资源/动作/结果/风险级/请求 ID/trace/correlation/changes，设计良好。

### 4.2 问题

**P0-③-A｜可信代理非 fail-closed，可静默记录 `127.0.0.1`（正是需求④点名的问题）**
- 证据：默认 `APP_TRUSTED_PROXIES=127.0.0.1/32,::1/128`（`config.go:258`），校验**只查格式**、空列表通过（`:371-375`）；`middleware/client_ip.go:16,28-35`；Gin 在"来源不在可信代理内"时直接返回 `RemoteIP()`。而生产 nginx 正是 host-local 反代（`basic-platform.conf.example:30,39` → `proxy_pass http://127.0.0.1:18080`）。
- 影响：`APP_TRUSTED_PROXIES` 漏配/显式置空时，**登录与操作审计的来源 IP 会被记成 `127.0.0.1`**（容器内 nginx 场景则记成 `172.x` 内网 IP），且事后无法区分。
- 另有三处模板默认值不一致：`platform/.env.example:21` 仅 loopback，而 `docker/.env.local.example:15` 与 `deploy/production/compose.yaml:223` 含 `172.16.0.0/12`。
- 整改：`Validate()` 强制 `TrustedProxies` 非空（或需显式 `APP_ALLOW_EMPTY_TRUSTED_PROXIES=true` 才允许为空）；统一三处模板；启动时打印生效代理列表；**加入部署后验收：登录一次并断言记录 IP 等于客户端公网 IP**。

**P0-③-B｜子系统自述的登录 IP 被无条件采信**
- 证据：平台摄取侧 `eventSourceIP` 只用 `net.ParseIP` 校验字面量（`audit/interfaces/http/handler.go:444-465`），缺失时回落为请求方（子系统容器）IP。
- 影响：任何持有 `audit.ingest` 凭据的发布方可写入**任意** IP（包括伪造成外网 IP），审计溯源链条不可信。
- 整改：平台侧对自述 IP 标注来源可信级别（自述 vs 平台观测），或在网关上补充独立观测；至少不要用自述 IP 做安全判定。

**P0-③-C｜4 个子系统审计上报 fire-and-forget，未配置即静默禁用**
- 证据：contract `router.go:297`（`_ = h.audit.Report`）、project `oidc.go:525` 与 `service_client.go:120-125`（env 缺失返回 nil）、data_analysis `audit.go:29-31`、Settlement `cmd/worker/main.go:47-48`（密钥缺失则 destination 不注册，事件**永久 PENDING**）。
- 影响：平台不可用或漏配 → 子系统操作完全无审计且**无任何告警**，直接违背"所有系统均有平台审计记录"。
- 整改：统一采用 outbox + 就绪探针强制（已有类似 `PLATFORM_AUDIT_REQUIRED` 开关的思路）。
- **对标基线**：`customer_and_opportunity` 是唯一同时具备"本地审计表 + 事务内 outbox + 持久化重试 + 失败登录安全事件"的子系统（`requestaudit/dispatcher.go`），应作为其余子系统的改造样板。

**P1-③-D｜失败登录大面积缺失、登录事件不带 IP**
- Settlement：全仓 **0 处** IP 提取，认证路径无任何审计（`internal/platform/auth.go:71-216` 无 audit 调用）。
- contract `oidc.go:584` 仅 `slog`；project `oidc.go:209-235` 各失败分支无上报；data_analysis `oidc/service.go:232,350` 构造事件未设 `UserLoginIP`。
- 影响：暴力破解/撞库无法发现与归因；登录审计无来源 IP，不满足需求。
- 整改：认证失败分支统一补 `auth.login.failed`（必须带 IP）；成功登录事件必须带 IP。

**P1-③-E｜平台采集 Keycloak 事件丢弃失败且结果硬编码**
- 证据：只取 `LOGIN`/`LOGOUT`、丢弃 `LOGIN_ERROR`（`keycloak_admin.go:126`），且 `result` 硬编码 `SUCCESS`（`event_audit.go:72`）。
- 影响：走 Keycloak 的 OIDC 登录失败在平台侧不可见，与 P1-③-D 叠加形成失败登录盲区。

**P1-③-F｜未认证请求不进入审计**
- 证据：`audit_trail.go:69-72` 无 principal 直接 return；子系统认证中间件先 `Abort()`，审计中间件不再执行。`auditResult` 的 `DENIED` 分支对 401 实际不可达。
- 影响：401/探测性请求无记录。

**P1-③-G｜审计无防篡改设计，且留存能力从未运行**
- 证据：`000007:44` 仅单行 `payload_hash`，无 prev_hash 链；`gorm_enhancement_repository.go:186` 执行 `Delete(&eventModel{})`；`RetentionService.Request` 全仓**无调用者**，且 `000038` 已移除留存权限 → 无 API/UI 可创建归档任务。
- 影响：有 DB 权限者可静默删除/修改且无法证明；留存事实上未运行（**同时意味着审计表将无限增长**，见 §8 容量）。
- 补救设计已存在：归档写 gzip NDJSON 并记 SHA-256、落盘后 `chmod 0440`（`worker/archive_writer.go:57-93`），只需接线。

**P2-③-H｜平台敏感数据读取不审计**
- 证据：`audit_trail.go:132-151` 仅记录写方法与 `/api/v1/audit/*`。
- 影响：查看用户手机号等敏感字段的读取行为无记录（对比：project/data_analysis 对 download/export 有敏感读审计，CRM 有联系人手机号访问审计）。

**P2-③-I｜子系统"私网即信任"无 CIDR 白名单**
- 证据：contract `router.go:390-400`、project `api.go:347-357`、data_analysis `audit.go:113-123`。
- 影响：私网内攻击者可伪造 XFF 得到任意**公网** IP；同时合法内网客户端 IP 因 `IsPrivate` 过滤被丢弃为空串。

**P2-③-J｜合规：脱敏不覆盖手机号/身份证，且 payload_hash 对未脱敏原文计算**
- 证据：`gorm_repository.go:634-637` 仅脱敏 `password/token/secret/cookie`；`:230-234` 的 `payload_hash` 由**未脱敏**输入计算，脱敏只作用于落库的 metadata/changes。
- 影响：若业务把完整手机号/身份证放入 metadata 将长期明文留存；哈希可用于离线比对。

---

## 5. 需求④：登录地址为外网 IP

### 5.1 机制正确（正面）

链路：客户端 → 宿主机 nginx（`X-Real-IP $remote_addr` + `X-Forwarded-For $proxy_add_x_forwarded_for`，`basic-platform.conf.example:16-19`）→ frontend 容器 nginx（同样透传，`frontend/nginx/default.conf:16-17`）→ 平台 API。
平台用 Gin 的"**从右向左跳过可信代理**"算法取客户端 IP。经推演：即使客户端自带伪造 `X-Forwarded-For: 1.2.3.4`，由于真实客户端 IP 会被 nginx 追加在右侧且不在可信网段，**返回的是真实客户端 IP**，伪造被忽略。设计正确。

### 5.2 问题

**P0-④-A｜无 fail-closed 校验**（同 P0-③-A）：漏配即记录 `127.0.0.1` 或内网 IP。

**P2-④-B｜登录落地地址校验接受 loopback**
- 证据：`application_management.go:638-648`（BaseURL 只要求 scheme+host）、`login_target.go:115-130`（相对路径放行）；dev 种子即为 `http://localhost:8081`（`000045:33`、`000052:11`、`000057:11`）。
- 影响："登录地址是外网 IP"在代码层**无任何技术保障**，仅靠部署时人工填写。
- 整改：生产环境校验拒绝 loopback/私网 BaseURL 与 LoginTarget；种子改为占位外网域名。

---

## 6. 需求⑤：业务流"已点击跑通、无报错"的复核

**我无法验证这条**（没有测试记录、用例清单或验收证据）。更重要的是，**这条验收方式已经被本次审计证明存在结构性盲区**：

> **反例（决定性）**：`project_management` 的站内通知链路 **100% 不可用**——scope 白名单不符必然 400、endpoint 不符必然 404、失败仅 `Warn`、且默认关闭。但"每个按钮点一遍、页面无报错"**完全成立**，因为失败被静默吞掉了。

由此可推出三类"点了没报错但功能实际不工作"的模式，需求⑤的验收方式**全程无法发现**：

| 模式 | 本次审计中的实例 |
| --- | --- |
| 跨系统集成静默失败 | PM 通知全 400/404；契约不统一（3 endpoint/2 scope） |
| 审计/上报 fire-and-forget | 4 个子系统审计上报失败无告警；Settlement 未配密钥则事件永久 PENDING |
| 后端"静默成功" | Settlement 开票签发无版本/无 RowsAffected 校验（P0-⑥-E） |

**建议**：把"点按钮无报错"升级为可重复的验收矩阵，至少包含：

1. **角色×操作矩阵**：每个角色实际能/不能做什么，与平台目录声明的权限码逐一比对（本次已发现 data_analysis 行级范围降级、contract admin 旁路这类"看起来能跑但越权"的问题）。
2. **跨系统链路断言**：不只点按钮，还要断言"对方系统收到了"（通知中心出现、审计有记录、业务状态在目标系统可见）。
3. **双人并发用例**（对应需求⑥）：两个账号同时改同一对象，断言冲突被正确拒绝或提示。
4. **异常路径**：重复提交、断网重试、平台不可用时的降级行为。
5. **负向断言**：确认失败时**有明确报错或告警**，而不是静默成功（对通知/审计这类副作用尤其重要）。

---

## 7. 需求⑥：并发修改同一对象

### 7.1 现状（差距极大）

| 仓库 | 判定 | 关键证据 |
| --- | --- | --- |
| platform | **完整防护** | `authorization/.../gorm_repository.go:213-218` role CAS + `RowsAffected!=1 → ErrVersionConflict`；identity/configuration/settings 同类；冲突→409 |
| customer_and_opportunity | **完整防护** | `opportunity/repository.go` 多处 `version=?`；`customer/repository.go:113-122` `IncrementProfileVersion` 作同事务末闸；`ErrVersionConflict → 409 COMMON_VERSION_CONFLICT` |
| contract_management | **部分防护** | 强：合同状态流转 `FOR UPDATE` + version 比对、审批规则 CAS；**弱：`con_contract_template` 无 version，`templates.go:67-69` 读后写无 CAS → 真丢失更新** |
| Settlement | **部分防护** | CAS 与 `FOR UPDATE`+原子自减覆盖确认/核销/冲销；**缺口：`operations.go:248` 开票签发 `UPDATE ... WHERE id AND tenant_id`，无 `version=?`、无 `RowsAffected` 校验 → 静默成功** |
| project_management | **无防护** | **16 张表零 version**；落库 `delivery.go:268`、`:338`、`repository.go:375` 全部无条件 `Updates`；`UpsertCapability` 全字段覆盖 |
| data_analysis | **无防护** | `alert_rule` 整表 `DELETE`+全量 `INSERT`（`admin/infrastructure/gorm_repository.go:165-175`）；`catalog.go:64` 是单调计数器非 CAS |

**跨仓库共性缺口**：
- 6 个仓库**全部没有** `If-Match`/`ETag`/**412**。
- 冲突语义不统一：`CON_VERSION_CONFLICT` / `COMMON_VERSION_CONFLICT` / `SETTLEMENT_CONFLICT`（把版本冲突与余额不足混码）/ `PM_STATE_CONFLICT`（仅语义冲突，无版本语义）。
- **前端盲区**：`project_management` 与 `data_analysis` 模块**各 0 处版本回传、0 处 409 处理**；settlement 仅 2 处回传版本、0 处冲突文案。即：即使后端补了版本号，这两个模块的用户也完全感知不到冲突。
- `project_management` **全仓无幂等键处理**：`pm_delivery_event` 无 `event_id` 唯一键，投递重试会写重复事件并二次执行状态机。
- contract_management 有一个**未被使用的防护**：后端唯一带 version CAS 的合同状态流转接口 `/contracts/:id/status-changes` **前端从未调用**。

### 7.2 高风险场景（按后果排序）

| 场景 | 当前行为 | 后果 |
| --- | --- | --- |
| 两人同时编辑同一合同模板 | 读后写、无 version、无 RowsAffected 校验 | **真丢失更新，双方都返回 200** |
| 两人同时操作同一结算单的开票签发 | `WHERE id AND tenant_id` 无条件更新 | **静默成功**，状态被覆盖 |
| 两人同时调整同一项目的服务项/实施计划 | 行锁串行不丢同字段，但**无版本信号** | 双方都看到成功，客户端拿不到"你基于旧版本"的信号 |
| 同一 delivery 事件重投到 PM | `pm_delivery_event` 无唯一键 | 审计流重复事件、状态机二次执行 |
| 两经理同改客户干系人/信息系统 | 靠同事务末尾 `IncrementProfileVersion` 兜底 | 正常路径安全，但**依赖调用方纪律**，新增子资源接口漏调即静默覆盖 |
| 两管理员同改角色权限 | role version CAS + 409 | ✅ 安全拒绝（可作为正面样例） |

### 7.3 最小整改（按性价比）

**P0**
1. `project_management` 补乐观锁：新增 migration 给 `pm_project`/`pm_service_item` 加 `version BIGINT UNSIGNED NOT NULL DEFAULT 1`；把 `delivery.go:268`、`:338`、`repository.go:375` 改为 `Where("tenant_id=? AND id=? AND version=?")` + `version=version+1`，`RowsAffected!=1 → 409`（HTTP 409 已存在，无需改路由）。
2. `con_contract_template` 加 version + `templates.go:67-69` 改 CAS（复用既有 `ErrVersionConflict` → `CON_VERSION_CONFLICT` 已映射）。
3. `pm_delivery_event` 加 `UNIQUE(tenant_id, event_id)` + `OnConflict{DoNothing}`。

**P1**
4. `Settlement operations.go:248` 补 `AND status='ISSUE_PENDING' AND version=?` + `RowsAffected!=1 → ErrConflict`；并把版本冲突从 `SETTLEMENT_CONFLICT` 拆出独立错误码。
5. `platform` `applicationaccess/service.go:628` 的 `DELETE authz_user_permission` 改条件删除 + 校验。
6. CRM 三张写热点表（`crm_approval_tasks`/`portal_project_messages`/`portal_filing_actions`）补 CAS 或至少 RowsAffected 校验。
7. **前端补齐**：`project_management`/`data_analysis` 模块回传 version + 处理 409（这是让后端改动生效的前提）。

**P2**
8. 统一冲突语义为 `409 + <子系统>_VERSION_CONFLICT`，在共享 request 层集中处理。
9. `project_management` 引入 `Idempotency-Key`。

---

## 8. 需求⑦：你没提到、但必须考虑的风险面

| # | 风险 | 现状与证据 | 建议 |
| --- | --- | --- | --- |
| 1 | **审计的可信度结构性依赖子系统自觉** | 业务流量**直连子系统、不经过平台**（`frontend/nginx/default.conf` 把各子系统 `/xxx/api/` 直连其自身后端）；平台只看到 OIDC、目录推送与审计上报。因此平台**无法独立观测**业务操作，子系统 bug 或被攻破后"不上报"即永久盲区 | 三选一：①业务流量经平台网关（可集中审计+集中鉴权，但集中负载）；②保留直连但加**上报缺口监测**（按应用做事件流心跳/断流告警）；③要求子系统审计上报带签名并做完整性对账 |
| 2 | **平台单点可用性** | 登录与授权上下文均依赖平台；降级语义未定义（§2.2 P1-①-D） | 明确 fail-open/fail-closed 语义并演练；评估平台 RTO 对全系统登录的影响 |
| 3 | **凭据生命周期无流程** | OAuth secret **不可回读**（列表/详情有专门测试断言不得泄露），轮换端点 `POST /oauth-clients/:id/credentials/rotate` 存在但无运维流程；机器凭据（`*_MACHINE_CLIENT_*`）同样 | 定义轮换周期与双凭据过渡（端点已支持 overlap/`valid_until`）；把轮换纳入发布流程 |
| 4 | **横向移动面** | 子系统间存在机器凭据（`CONTRACT_MACHINE_*`、`PROJECT_MACHINE_*`）；data_analysis 还持有 `CONTRACT_READ_DSN`/`CRM_READ_DSN` 直连其它系统业务库**只读** | 逐一审计机器凭据 scope 是否最小化；确认只读 DSN 的库账号确无写权限；评估"一个子系统被攻破能否读全量业务数据" |
| 5 | **审计/通知表无限增长** | 留存能力从未运行（`RetentionService.Request` 无调用者，`000038` 移除了留存权限）→ `audit_event` 只增不减；通知表同理 | 接线归档（实现已存在：gzip NDJSON + SHA-256 + `chmod 0440`）；设定保留期与容量告警 |
| 6 | **数据一致性无对账** | `notification_user_stat.unread_count` 与 `notification_delivery.read_at` 靠同事务维护，但无漂移对账任务；Settlement 催收同时写本地表与平台（可能重复展示） | 加周期性对账/修复任务；明确"本地收件箱 vs 平台收件箱"的去重归属 |
| 7 | **合规与隐私** | 脱敏仅覆盖 password/token/secret/cookie，**不含手机号/身份证**；`payload_hash` 对未脱敏原文计算；审计导出已有权限门（`platform:audit:export`） | 扩脱敏字段清单；`payload_hash` 改基于脱敏后内容或去掉 |
| 8 | **发布与迁移安全** | 仓库内已有多次事故记录文档（disk-full 登录中断、claims 修复、catalog hash 修复），说明历史上出过生产问题；迁移的在线 DDL/回滚演练要求写在文档里但需持续执行 | 保持"迁移必须空库+增量+回滚演练"的门禁；把本次审计项纳入发布前检查表 |
| 9 | **前端安全姿态** | 正面：会话在 Cookie（非 localStorage 存令牌）、写操作带 CSRF 标记；但标记是硬编码 `'X-CSRF-Token': '1'`，强度**完全依赖后端**对 Origin/`Sec-Fetch-Site` 的校验 | 确认后端该校验的实现与覆盖范围；考虑升级为标准 double-submit token |
| 10 | **可观测性** | 正面：审计与通知均带 request_id/trace_id/correlation_id | 把 trace 贯穿到跨系统链路（通知→目标系统审计），便于端到端溯源 |

---

## 9. 整改路线图

按"**先堵漏洞、再通链路、后统一契约**"排序。P0 项均为小改动、高收益。

### 阶段一（P0，建议 1–2 周）：堵住"静默失效"与"真丢失更新"

| 项 | 内容 | 验收 |
| --- | --- | --- |
| 1 | PM 通知链路修正：scope→`CROSS_SYSTEM`、endpoint 统一、新建 outbox + worker、失败可观测 | 触发一次项目指派，**平台通知中心能看到**该通知；失败时日志/指标可见 |
| 2 | Settlement 目录推送补实现（`cmd/authz-catalog`） | 平台授权中心能看到 Settlement 的 4 个角色与 12 个权限 |
| 3 | data_analysis 目录去硬编码（单一数据源读 manifest）+ `cmd/authz-catalog` 落实 | CI 断言 Go 目录与 `permission-manifest.yaml` 完全一致 |
| 4 | `APP_TRUSTED_PROXIES` fail-closed + 三处模板统一 + 启动打印生效列表 | **部署后登录一次，断言审计/登录记录 IP == 客户端公网 IP，且非 127/内网** |
| 5 | 审计上报改 outbox（对标 customer_and_opportunity）+ 就绪探针强制 | 平台不可用时事件本地留存并在恢复后补投 |
| 6 | PM 加 `version` + 三处 CAS + `pm_delivery_event` 唯一键 | 双人并发改同一服务项，后者收到 409 并看到冲突提示 |
| 7 | `con_contract_template` 加 version + CAS | 双人并发改同一模板，后者 409 |
| 8 | Settlement `operations.go:248` 补条件更新 + RowsAffected 校验 | 并发签发同一开票申请，第二次被拒且**不是静默成功** |
| 9 | data_analysis 行级范围改为取自授权上下文 | `sales_director`/`tech_director` 只能看到本组织/本团队数据 |

### 阶段二（P1，建议 3–5 周）：补齐覆盖与用户体验

- data_analysis 建通知 outbox + 告警接入（`alert_rule_recipient` 启用）
- Settlement 补审批/开票/收款/核销通知（含**申请人**）
- contract 补失败登录审计 + 登录事件带 IP；project/data_analysis 同
- 平台：投递重试 worker、摄取 attempt 上限+退避+DEAD 告警、未认证请求审计、Keycloak `LOGIN_ERROR` 采集、收件箱权限与未读角标、敏感 GET 读审计
- 前端：`project_management`/`data_analysis` 回传 version + 409 处理；settlement 补冲突文案
- contract 删除 admin 硬编码旁路

### 阶段三（P2，建议 6–10 周）：统一契约与合规

- 通知契约统一（1 endpoint + 1 scope）+ 在 sharedDocs 固化；跨系统提醒矩阵落地
- 冲突语义统一（`<子系统>_VERSION_CONFLICT`）+ 共享前端处理层
- 审计留存接线（归档 + 保留期 + 容量告警）、防篡改链、脱敏字段扩展
- 凭据轮换流程落地；机器凭据 scope 审计
- 平台降级语义文档化 + 演练；审计缺口监测（§8-1）

---

## 10. 验收清单（可直接执行）

1. **登录收敛**：用错口令登录任一子系统 → 无本地校验路径；审计中出现带**公网 IP** 的 `auth.login.failed`。
2. **外网 IP**：部署后登录，断言记录 IP == 客户端公网 IP，且**不等于** 127.* 或 172.16/12。
3. **目录一致**：对每个子系统执行目录发布，平台授权中心的角色/权限码与仓库 `authz/permission-manifest.*` **逐条一致**（含 Settlement 与 data_analysis）。
4. **跨系统通知**：触发一次"项目指派"，断言平台通知中心出现该通知且**被指派人**可见（不只是操作者）。
5. **审计覆盖**：逐个高风险操作（审批、状态流转、导出、权限变更）执行后，断言平台 `audit_event` 有对应记录；**并在平台宕机时验证事件不丢**（本地 outbox 留存、恢复后补投）。
6. **并发**：两个账号同时编辑同一合同模板 / 服务项 / 结算单开票，断言后提交者收到 409 且**前端有明确提示**。
7. **越权**：用 `sales_director`/`tech_director` 登录 data_analysis，断言看不到超出其组织/团队范围的数据。
8. **投递可靠性**：人为让平台通知接口返回 5xx，断言投递事件**不会永久占队头**、有退避与重试计数、超限后进死信并可查。

---

## 11. 附：证据索引（重点文件）

| 主题 | 关键文件 |
| --- | --- |
| 权限判定 | `platform/internal/transport/http/middleware/authorization.go:14-31` |
| 目录接收 | `platform/internal/transport/http/router.go:546`、`internal/platform/authorization/applicationaccess/handler.go:290`、`catalog.go:150-304` |
| 客户端 IP | `platform/internal/transport/http/middleware/client_ip.go:12-35`、`internal/shared/config/config.go:258,371-375` |
| 审计摄取 | `platform/internal/transport/http/router.go:560-561`、`internal/platform/audit/interfaces/http/handler.go:444-465` |
| 通知摄取 | `platform/internal/transport/http/router.go:566-568`、`internal/platform/notification/application/ingestion.go:83,143` |
| CAS 正面样例 | `platform/internal/platform/authorization/infrastructure/gorm_repository.go:213-218`、`customer_and_opportunity/internal/infrastructure/mysql/customer/repository.go:113-122` |
| 丢失更新样例 | `contract_management/internal/infrastructure/mysql/templates.go:67-69`、`project_management/internal/infrastructure/mysql/delivery.go:268,338` |
