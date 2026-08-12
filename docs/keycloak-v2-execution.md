# Keycloak 全量接入 V2：执行基线

本文把《全量子系统 Keycloak 接入—详细实施方案 V2》落实为本仓库的执行约束。
它不是一次性全量切换说明；所有认证切流都必须按应用、环境分别完成并可回滚。

## 目标职责边界

| 边界 | 职责 |
| --- | --- |
| 基础平台 | 用户、组织、岗位、业务角色目录、岗位模板、个人例外授权、最终权限计算与审计事实源。 |
| Keycloak | Realm、Client、浏览器登录、SSO、会话、Token 签发，以及平台授权结果的单向投影。 |
| 子系统 | 校验 Keycloak Token，并在本地执行业务权限和数据范围。 |

Keycloak 中的 Client Role 和 Claims 只是令牌投影载体；不得在 Keycloak 直接编辑业务人员授权，也不得反向同步到基础平台。

## 已确认的迁移模型

子系统作为 Keycloak RP。Keycloak 可把基础平台作为上游 Broker，以兼容现有身份数据；但浏览器和子系统信任的 Issuer 均是 Keycloak Realm。

服务端运行配置必须一起更新，不能只改 `OIDC_ISSUER`：

```text
公开 Issuer       KEYCLOAK_PUBLIC_URL/realms/<realm>
容器后通道         http://keycloak:8080
Client ID / Secret 由认证接入控制面受控写入
Redirect URI       由应用环境 Public BaseURL + PathPrefix 推导并校验
Cookie Secure      与 HTTPS 门禁一起更新
```

## 强制安全规则

1. 禁止使用全局 `SUBSYSTEM_OIDC_ISSUER` 或全局后通道覆盖多个应用环境；每个 `runtime/*.env` 是该环境唯一 OIDC 来源。
2. `issuer_alias` 只允许 `platform` 或 `keycloak`。它是切流状态，不能由通用运行时编辑接口随意写入。
3. 先同步 Client、角色目录、授权投影并完成真实 Broker 登录验证，再允许切换。
4. 切换/回滚由服务端固定目标认证提供方；浏览器不得提交可控 `issuer_alias`。
5. Client Secret、Keycloak 管理凭据、Token 和环境 metadata 均不得在目录读取 API 或前端状态中返回。
6. Keycloak 切换始终校验 Public BaseURL、推导的 Redirect URI 与 Cookie 策略一致。当前 `KEYCLOAK_REQUIRE_HTTPS=false` 保持受控 HTTP 兼容；只有明确启用该开关后，才强制要求 TLS 与 Secure Cookie。

## HTTPS 切换前检查

当前部署可以继续使用受控 HTTP。`KEYCLOAK_REQUIRE_HTTPS=false`（默认）时，Keycloak Client 同步与首次切换会校验 Public BaseURL、推导出的 Redirect URI 和运行时 Cookie 策略一致：HTTP 入口对应 `Secure Cookie=false`，HTTPS 入口对应 `Secure Cookie=true`。

待入口网关、所有 Redirect URI 和 Cookie 一并迁移到 TLS 后，设置 `KEYCLOAK_REQUIRE_HTTPS=true`。此后 HTTP 的 Keycloak Client 同步或首次切换都会在平台侧被拒绝，运行时配置不会下发。该开关不会自动改写既有环境，也不会自行开启服务器 HTTPS。

## 逐环境状态机

```text
PLATFORM_ACTIVE
  -> CLIENT_SYNCED
  -> PROJECTION_PENDING
  -> BROKER_LOGIN_VERIFIED
  -> OBSERVING
  -> OBSERVATION_COMPLETE
  -> KEYCLOAK_ACTIVE (含回滚截止时间)
  -> PLATFORM_ACTIVE
```

`CLIENT_SYNCED` 不是切流成功；切流必须同时满足 Client、角色目录、用户授权投影、真实登录验证四项门禁。观察期、回滚截止时间及失败原因必须持久化并可审计。

平台的受控时间策略为：门禁全部通过后由管理员显式启动 **7 天观察期**；观察截止前，`/switch` 会以 `IAM_AUTH_PROVIDER_SWITCH_OBSERVATION_REQUIRED` 拒绝请求且不会下发运行时配置。切换成功后再开启 **7 天回滚窗口**；窗口内可通过专用回滚接口回到基础平台 OIDC，窗口过期后必须走新的受控变更，而不是绕开审计修改环境字段。

## FAILED 投影、告警与受控重放

Keycloak 授权投影最多自动尝试 5 次。仍失败的事件进入 `FAILED` 死信状态：它会阻断对应应用环境切换，但不会删除平台的授权事实或暴露 Client Secret/Token。

认证接入页提供以下受控操作：

```text
GET  /api/v1/keycloak-integration/projection-alerts
GET  /api/v1/keycloak-integration/projection-failures?application_code=&environment=
POST /api/v1/keycloak-integration/projection-failures/{event_id}/replay
```

重放必须同时满足：事件仍为 `FAILED`、操作者具有应用/环境/角色绑定更新权限、`confirmation` 与事件 ID 完全一致，并填写至少 6 个字符的处置原因。重放只做一次原子 `FAILED -> PENDING`，不会新建重复投影任务；Worker 成功后才重新放开用户投影门禁。

## Keycloak 事件审计

Worker 使用 Keycloak Admin API 的标准 `/events` 与 `/admin-events` 接口拉取登录、登出和管理员事件。读取使用短时间重叠窗口，基础平台按 `keycloak:<event_id>` 去重，因此重启和轮询重复不会造成重复审计。

事件只会在 `iam_account.external_subject_id` 能映射到活动平台账号时写入相应租户的 `audit_event`；无法确定租户的事件会安全跳过，而不是错误归属。Keycloak Admin Service Account 必须被授予该 Realm 最小的 `view-events`、`view-realm` 权限；现有的 Client/用户管理权限保持不变。

## 切换顺序

默认顺序是单个应用、单个环境依次切换，而不是全量并行。既有系统按依赖关系进行如下灰度：

1. 客户与商机管理（CRM）：先在一个低风险环境完成 Client/Claims/角色投影和 Broker 登录验证，再观察、切换；Worker 与 API 使用同一环境运行时配置一起验证。
2. 客户自助门户：仅在 CRM 对应环境已完成回归验证后推进，重点验证门户身份映射、邀请、退出登录与 CRM 依赖路径。
3. 项目管理：独立完成单环境切换与项目业务回归。
4. 合同管理：最后验证 OIDC 登录、Temporal Worker、审批与机器客户端仍保持独立凭据。

每个环境完成：同步 -> 投影完成 -> 登录验证 -> **7 天观察** -> 切换 -> 业务/工作流回归 -> **回滚窗口观察**，才可以开始下一个环境。基础平台只提供操作入口、门禁与审计，不会自行切换任何 CRM、门户、项目或合同环境。

## 兼容与退役

已接入且仍使用基础平台 OIDC 的环境继续可用。旧平台 OIDC 与浏览器 Client 只能在所有依赖环境完成 Keycloak 切换、回滚窗口结束并完成审计确认后退役；不得为“代码整洁”提前删除。
