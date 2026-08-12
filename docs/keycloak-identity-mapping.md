# Keycloak 身份与授权映射

基础平台是企业数字身份、组织、岗位和最终授权的唯一事实来源。Keycloak 是统一登录入口与 OIDC Token 中继层，不得在 Keycloak 控制台手工赋予业务角色或权限来绕过基础平台。

## 映射契约

Keycloak Broker 从基础平台 OIDC UserInfo 导入以下属性，并在 Keycloak Realm Client 的 ID Token、Access Token 与 UserInfo 中原样输出：

| 基础平台声明 | Keycloak 用户属性 / Token Claim | 含义 |
| --- | --- | --- |
| `identity_id` | `identity_id` | 企业内唯一数字身份 ID（`iam_user.id`） |
| `tenant_id` | `tenant_id` | 租户边界 |
| `person_id` | `person_id` | 人员档案 ID；可为空 |
| `primary_org_id` | `primary_org_id` | 主组织 |
| `organization_ids` | `organization_ids` | 所属组织集合 |
| `roles` | `roles` | 基础平台计算出的有效业务角色集合 |
| `permissions` | `permissions` | 基础平台计算出的有效权限集合 |
| `role_config_hash` | `role_config_hash` | 角色目录版本摘要 |
| `authz_revision` | `authz_revision` | 用户授权修订号 |

`organization_ids`、`roles` 与 `permissions` 是多值 JSON 声明；其余为单值声明。角色与权限以 Keycloak 用户属性和 Token Claim 的形式镜像，不转换成可由管理员手工编辑的 Realm Role。这样可避免两套授权源发生漂移。

## 同步与生效

在基础平台“应用接入管理”执行“同步 Keycloak”时，平台会校验并补齐 Broker 属性、Identity Provider Mapper 与各 Realm Client Claim Mapper。基础平台上的组织、岗位、角色或个人例外授权变更后，以最新 `authz_revision` 和 `role_config_hash` 为准；子系统不得仅根据旧 Token 长期缓存权限。

## 子系统认证接入

Keycloak 管理子系统的 Realm Client、回调地址、Client Secret、会话和 Token 签发；基础平台不再作为子系统认证 Client 的事实源。基础平台仍保存应用目录、访问入口和授权投影，以便计算最终角色与权限并同步到 Keycloak。

- **新接入环境**：当 `SUBSYSTEM_DEFAULT_ISSUER_ALIAS=keycloak` 且 Keycloak 管理能力已启用时，接入页面默认选择 Keycloak。平台会创建或更新 `<application_code>-<environment>-web` Client，并将受控运行时配置交给部署 Agent。
- **已有环境**：不会因修改默认值被隐式切换。管理员必须在该环境依次执行“同步 Keycloak”与“切换 Keycloak”；系统确认 Client、角色目录、用户投影和 Broker 登录四项门禁后才写入新的 Issuer。
- **回滚**：若目标应用健康检查或登录验证失败，使用“回滚基础平台”恢复该环境原有基础平台 OIDC 配置；不会删除 Keycloak Client 或平台授权数据。

Client Secret 只在服务端、Keycloak 和受控部署 Agent 之间传递。页面只能展示是否已配置，不能读取或导出明文。不要在 Keycloak 控制台手工给用户写入业务角色或权限；这会绕开基础平台的岗位、组织和个人例外授权计算。

## Keycloak 故障策略

策略固定为 `continue_existing_platform_sessions`：

- 已建立的基础平台浏览器会话继续可用，直到其原有过期、空闲超时、主动退出或被平台撤销。会话签发、签名和校验均由基础平台及其数据库完成，不依赖 Keycloak。
- 不允许通过缓存的 Keycloak Token、过期 Token 或无法实时校验的外部 Token 创建新的基础平台会话。
- Keycloak 不可用时，新的 Keycloak Broker 登录、Realm Client 同步和认证提供方切换均失败关闭；管理员应修复 Keycloak 后重试。
- 使用 Keycloak 作为 Issuer 的子系统能否继续访问取决于其自身已建立会话与本地 JWKS 缓存策略；这不扩大基础平台的会话授权范围。
