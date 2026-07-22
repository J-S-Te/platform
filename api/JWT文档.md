# Basic-Platform JWT 文档（当前实现）

> **用途**：说明当前后端实际签发、校验和使用的 JWT，不把“所有令牌都是 JWT”作为前提。
> **代码基线**：`backend/internal/shared/security/{jwt.go,application_jwt.go,oidc_jwt.go}`，以及对应认证中间件和 OIDC 适配器。
> **生成日期**：2026-07-21。

## 1. 令牌总览

当前实现至少存在三类已签名 JWT，以及若干明确不是 JWT 的机密值。

| 类型 | 签发/校验组件 | 用途与传输 | `aud` | `token_use` |
|---|---|---|---|---|
| 浏览器会话 JWT | `JWTManager` | 写入 HttpOnly session Cookie；控制台受保护接口只从该 Cookie 读取 | `AUTH_JWT_AUDIENCE` | 无 |
| 应用 access token | `ApplicationJWTManager` | OAuth client-credentials 后返回，调用方以 `Authorization: Bearer` 发送 | `AUTH_APPLICATION_JWT_AUDIENCE` | `application` |
| OIDC access token / ID token | `OIDCJWTManager` | `/oauth2/token` 的 OIDC 授权码或刷新令牌流程签发；JWKS 可公开获取公钥 | 目标 OAuth `client_id`（数组语义） | `access_token` 或 `id_token` |

**不是 JWT 的值**：刷新令牌、OAuth 授权码、客户端密钥、MFA challenge、MFA step-up grant、登录 pre-auth credential、联邦登录 state，以及钉钉授权结果/授权码/上游令牌。持久化模型为这些值设计了哈希或密文字段，不能把它们误当成可在客户端解析的 JWT。钉钉原始 `unionId` 仅在进程内立即哈希查找预绑定身份，不存库、不记日志或审计。

### 1.1 钉钉扫码与平台 JWT 的边界

钉钉第三方企业应用扫码的 `state`、授权码、授权结果、上游访问令牌、浏览器绑定 Cookie 与 SuiteSecret/客户端密钥都不能作为平台 JWT、平台 Cookie 载荷或前端可解析令牌使用。创建扫码会话时，state 只在发起浏览器收到的 `sdk_config` 中出现；MySQL 的 `iam_dingtalk_qr_login_state` 仅保存 `state_hash` 与受 `IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY` 保护的服务端载荷，原始 state 和浏览器绑定值不持久化。回调仅接受已预绑定且有效的本地身份；官方 DTFrameLogin SDK 成功后由顶层页面导航后端 callback，callback 以 303 跳转结束：不需要 MFA 时，后端创建真实服务端会话并按 `AUTH_SESSION_COOKIE_*` 写既有浏览器会话 JWT Cookie；需要 MFA 时仅写既有预认证 Cookie，完成 MFA 后才创建会话。跳转 URL、响应、日志、审计和 Cookie 均不得包含钉钉敏感协议值。

## 2. 共同的密码学与密钥要求

### 2.1 算法与紧凑序列化

- 三类 JWT 均为 JWS Compact Serialization：`base64url(header).base64url(payload).base64url(signature)`。
- 签名算法只接受 **EdDSA / Ed25519**。实现拒绝非 `alg=EdDSA` 或非 `typ=JWT` 的头部。
- 运行时加载私钥要求 **PKCS#8 Ed25519 PEM**；公钥要求 **PKIX Ed25519 PEM**。
- 浏览器会话 JWT 启动时还验证私钥派生公钥与配置公钥一致；OIDC 和应用 JWT 同样基于配置的 Ed25519 密钥加载。
- 三类 manager 在 bootstrap 中由同一组 `AUTH_JWT_PRIVATE_KEY_PATH`、`AUTH_JWT_PUBLIC_KEY_PATH` 创建，但各自使用不同的 audience / claim 约束。

### 2.2 必要配置

| 配置 | 作用 |
|---|---|
| `AUTH_JWT_ISSUER` | 浏览器会话 JWT 与应用 JWT 的 `iss`。 |
| `AUTH_JWT_AUDIENCE` | 浏览器会话 JWT 的 `aud`。 |
| `AUTH_APPLICATION_JWT_AUDIENCE` | 应用 JWT 的 `aud`。 |
| `OIDC_ISSUER` | OIDC token 的 `iss`；必须是没有 query/fragment 的绝对 origin URL。 |
| `AUTH_JWT_PRIVATE_KEY_PATH` | Ed25519 PKCS#8 私钥 PEM 路径。 |
| `AUTH_JWT_PUBLIC_KEY_PATH` | Ed25519 PKIX 公钥 PEM 路径。 |
| `AUTH_SESSION_COOKIE_NAME` | 浏览器会话 Cookie 名；默认配置为 `bp_session`。 |
| `AUTH_SESSION_COOKIE_SECURE`、`AUTH_SESSION_COOKIE_SAME_SITE`、`AUTH_SESSION_TTL` | Cookie 属性和会话有效期。`SameSite=None` 时必须启用 Secure。 |

## 3. 浏览器会话 JWT

### 3.1 Header 与 Claims

```json
// header
{ "alg": "EdDSA", "typ": "JWT" }
```

| Claim | 含义 | 校验 |
|---|---|---|
| `iss` | `AUTH_JWT_ISSUER` | 必须与 `JWTManager` 配置精确相等。 |
| `aud` | `AUTH_JWT_AUDIENCE` | 必须与 `JWTManager` 配置精确相等。 |
| `sid` | 会话 ID | 非空。之后认证服务会根据会话状态建立可信主体。 |
| `sub` | 用户 ID | 非空。 |
| `tid` | 租户 ID | 非空。 |
| `aid` | 账号 ID | 非空。 |
| `iat` | 签发秒级 Unix 时间 | 不得晚于当前时间 + 1 分钟容差。 |
| `exp` | 到期秒级 Unix 时间 | 必须严格晚于 `iat` 且严格晚于当前时间。 |

### 3.2 传输与服务端认证顺序

1. 登录或 MFA 登录成功后，服务端把此 token 设为 HttpOnly session Cookie；成功响应 `data` 只返回 `expires_at` 与 `redirect_url`，不回显 token。
2. `Authentication` 中间件只调用 `request.Cookie(cookieName)` 读取 Cookie；缺失或校验失败返回 `401 AUTH_UNAUTHENTICATED`。
3. JWT 签名、格式、header、issuer/audience、必填 claims、时间都通过后，认证服务还会解析当前 session / 用户 / 账号 / 权限，构造 `authctx.Principal`。
4. 路由级 `RequirePermission` 再根据可信 Principal 的 `permission_codes` 判定，不从浏览器传入的 ID 或 header 建立身份。

### 3.3 刷新与注销

- `POST /api/v1/auth/token/refresh` 必须已有有效 Cookie；服务端更新当前会话，并下发替换 Cookie。
- `POST /api/v1/auth/logout` 撤销当前服务端会话并清除 Cookie。
- JWT 自身有效不代表服务端 session 一定仍有效；认证服务会进行存储状态校验。因此调用方不能仅离线验证 session JWT 后视为已获授权。

### 3.4 外部身份登录后的会话边界

- 当前 `GET /api/v1/auth/external/callback` 在完成外部 **OIDC** 回调验证与本地账号绑定后，成功时只通过重定向写入平台 session Cookie；若账号要求 MFA，则写入独立的短期 HttpOnly MFA 前置 Cookie。
- 上游授权码、外部 `id_token`、外部 access token、联邦登录 `state` 以及 MFA 前置凭据都不是本平台 JWT，也不会写入浏览器响应 JSON。它们不得记录在日志、审计详情或浏览器持久化存储中。
- 因此，外部身份认证只是平台会话建立前的身份确认步骤；后续平台受保护接口仍按本节的浏览器 session JWT 与服务端 session 状态完成认证。

### 3.5 `DINGTALK_QR` 与 JWT 的边界

- `DINGTALK_QR` 是独立的联邦身份提供商类型，不属于现有外部 OIDC 路由。扫码登录由 `POST /api/v1/auth/dingtalk/qr-sessions` 与 `GET /api/v1/auth/dingtalk/callback` 承担；短时、一次性扫码状态表已由 `000031_create_dingtalk_qr_login_state.sql` 创建。
- 钉钉授权结果、授权码和上游令牌都不是平台 JWT，也没有钉钉专属 JWT、claim、audience 或 token 响应。登录页使用官方 DTFrameLogin SDK 获取授权结果后，由顶层页面导航 callback；callback 只以 `303 See Other` 结束。
- 回调在校验 state 摘要、固定 `SameSite=Lax` 的浏览器绑定、有效期、一次性消费状态、钉钉身份、预绑定账号和账号状态后，复用既有 `SessionIssuer`。普通成功仅写入既有控制台会话 Cookie；需要 MFA 时仅写入既有预认证 Cookie 并跳转至 `/login?dingtalk_mfa=1&return_to=...`。
- 钉钉 SuiteSecret/客户端密钥的持久化载体是提供商的 `client_secret_ciphertext`，而不是 JWT claim；原始 `unionId` 只在进程内立即计算 SHA-256 哈希以查询预绑定身份，不存库、不记日志、不写审计详情，也不能成为平台 JWT claim。跳转地址、响应、Cookie、日志和审计详情均不得包含 state 原文、授权码、SuiteSecret/客户端密钥、上游令牌或原始外部身份标识。

## 4. 应用 JWT（client credentials）

### 4.1 Header 与 Claims

Header 为 `{ "alg":"EdDSA", "typ":"JWT" }`。Payload 如下：

| Claim | 含义 | 当前校验要求 |
|---|---|---|
| `iss` | `AUTH_JWT_ISSUER` | 精确匹配。 |
| `aud` | `AUTH_APPLICATION_JWT_AUDIENCE` | 精确匹配。 |
| `token_use` | 固定 `application` | 必须为 `application`。 |
| `sub` | OAuth `client_id` | 非空。 |
| `oauth_client_id` | OAuth 客户端内部 ID | 非空。 |
| `tenant_id` | 客户端所属租户 | 非空。 |
| `application_id` / `application_code` | 已注册应用 ID / code | 非空。 |
| `environment_id` / `environment_code` | 已注册环境 ID / code | 非空。 |
| `scope` | scope 字符串数组 | 至少一个、每项非空、不可重复。 |
| `iat` | 签发秒级 Unix 时间 | 不得在未来。 |
| `nbf` | 生效秒级 Unix 时间 | 缺省时签发器设置为 `iat`；不得晚于 `exp`。 |
| `exp` | 过期秒级 Unix 时间 | 必须晚于 `iat` 且当前未过期。 |

### 4.2 Bearer 使用与二次校验

```http
Authorization: Bearer <application-access-token>
```

- `ApplicationAuthentication` 中间件要求 `Authorization` 恰好为一个非空 Bearer token；格式不对或校验失败均为 401。
- JWT manager 验证完成后，应用注册服务还会重新核对 token 里的 OAuth client、tenant、application、environment 绑定和 scope 是否仍与活动注册一致。
- 当前 router 中采用该边界的端点是：`POST /api/v1/audit/events` 与 `POST /api/v1/audit/events:batch`；两者额外要求 `audit.ingest` scope。
- **不要**用浏览器 session token 调用这些接入端点，也不要把应用 token 放进 Cookie。

## 5. OIDC JWT（access token 与 ID token）

### 5.1 OIDC Header / JWKS

```json
{ "alg": "EdDSA", "typ": "JWT", "kid": "<derived-key-id>" }
```

- OIDC manager 的 `kid` 由当前 Ed25519 公钥确定；验证器要求 header 的 `kid` 精确匹配当前 manager。
- `GET /oauth2/jwks` 暴露一个 OKP 公钥：`kty=OKP`、`crv=Ed25519`、`use=sig`、`alg=EdDSA`、`kid` 与 base64url `x` 公钥。
- 调用方可通过 `GET /.well-known/openid-configuration` 发现 JWKS URI 和其他 OIDC 元数据；不要硬编码键值。

### 5.2 OIDC claims

| Claim | 含义 |
|---|---|
| `iss` | `OIDC_ISSUER`。 |
| `sub` | 用户 ID。 |
| `aud` | client ID 数组语义；解析器也接受单个字符串形式。 |
| `iat` / `exp` | 秒级 Unix 时间。签发时截断至秒。 |
| `jti` | token ID。 |
| `sid` | 会话 ID。 |
| `auth_time` | 授权时的认证时间。 |
| `scope` | 空格分隔、规范化的 scope 字符串。 |
| `client_id` | OAuth client ID。 |
| `nonce` | 授权请求 nonce。 |
| `token_use` | `access_token` 或 `id_token`。 |

当前签名器拒绝以下不一致：issuer 不匹配、token use 与签发方法不匹配、空或重复 audience/scope、`exp <= iat`、`auth_time > iat`、不规范的 scope 格式、无效 header 或签名。

### 5.3 UserInfo 的额外在线约束

`/oauth2/userinfo` 不仅验证 OIDC access token 的签名和 claims：还会通过 `client_id`、`sid`、`sub` 查询活动 client 与会话，并要求 client 的 tenant 与 session subject 的 tenant 一致、session user 与 token `sub` 一致。故 OIDC access token 也不是纯离线授权凭据。

## 6. 时间、撤销和轮换注意事项

- **浏览器 session token**：`exp` 是必要条件，但服务端会话状态仍决定最终可用性；logout/禁用等状态变化可使 token 失效。
- **应用 token**：除签名与时间外还依赖 OAuth 客户端注册的在线检查；修改客户端状态或 scope 后，旧 token 不应被视为天然可用。
- **OIDC refresh token 与授权码**：当前数据模型保存哈希、消费和撤销状态，而不是 JWT。交换、刷新、撤销依赖服务端存储记录。
- **密钥轮换**：当前 `OIDCJWTManager.JWKS()` 返回当前一把公钥。代码没有在同一个 JWKS 响应中维护多个历史 `kid` 的轮换集合；部署轮换时应先评估仍未过期的 OIDC token 与依赖方缓存行为，不能假定多 key 并行已实现。

## 7. 安全实现要求

1. 不记录完整 JWT、refresh token、客户端密钥、密码、MFA secret / challenge / grant；日志中如需关联请使用服务端 request/session ID，而不是凭据本身。
2. 禁止接受 `alg=none` 或把 JWT header 中的 algorithm 当成协商结果；当前实现固定 EdDSA。
3. 每个消费方都必须校验自己的 issuer、audience、token type 和时间，不可只解码 payload。
4. 控制台前端使用 Cookie 凭据与 CSRF/SameSite 策略；不要把 session JWT 保存到 localStorage/sessionStorage。
5. 业务服务若新增 Bearer 接口，应显式决定使用哪类 token，并在中间件中完成注册/会话在线校验；不能复用浏览器 Cookie 验证结果。

## 8. 相关源码定位

| 关注点 | 源码 |
|---|---|
| 浏览器 JWT 签发 / 校验 | `backend/internal/shared/security/jwt.go` |
| 应用 JWT 签发 / 校验 | `backend/internal/shared/security/application_jwt.go` |
| OIDC JWT、JWK 与 JWKS | `backend/internal/shared/security/oidc_jwt.go` |
| Cookie 会话认证 | `backend/internal/transport/http/middleware/authentication.go` |
| 应用 Bearer 认证与 scope | `backend/internal/transport/http/middleware/application_authentication.go` |
| MFA step-up grant 消费 | `backend/internal/transport/http/middleware/mfa_step_up.go` |
| OIDC token / JWKS / UserInfo HTTP 端点 | `backend/internal/platform/oidc/interfaces/http/` |
| 联邦提供商类型、敏感字段边界 | `backend/internal/platform/identity/federation/domain/model.go` |
| 钉钉提供商配置校验（非扫码回调） | `backend/internal/platform/identity/federation/application/service.go` |
| 钉钉扫码状态、身份换取与既有会话签发边界 | `backend/internal/platform/identity/federation/dingtalk/application/service.go`、`backend/internal/platform/identity/federation/dingtalk/infrastructure/` |
