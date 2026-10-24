# Keycloak 身份、授权投影与切换操作

> 源码核验日期：2026-10-13；未执行真实 Broker 登录、观察期或运行时切换。

## 事实来源与投影

基础平台是身份、组织、岗位及最终授权来源，Keycloak 提供 Broker 登录和 OIDC Client/Token 能力。不要在 Keycloak 控制台手工维护业务角色来绕过平台。

完整投影由事务 outbox → Worker → 最新平台快照 → 确保用户/组织 Group → 同步目标 Client Role → 应用/Client 范围授权属性 → 标记同步完成组成。实现实际创建/分配 Client Roles，并清理目标 Client 的陈旧映射；不是“只写用户属性、不转换角色”。不同应用不能共用全局 roles/permissions 属性互相覆盖；权限、hash、revision 的来源仍是平台。

稳定 identity_id、tenant_id、人员/主组织及组织集合与应用授权属性按 Mapper 输出；不要把 Keycloak subject 当成平台用户 ID。基础平台上游标准 `/oauth2/userinfo` 是 Broker 所需能力；关闭旧平台 Token 直接调用 authorization-context 的兼容开关不等于关闭 UserInfo。

## 管理员步骤与门禁

1. 具有应用/环境更新权限的管理员，在“应用接入”中选择精确环境，先登记目录并执行“同步 Keycloak”。这不等于已部署应用、已登录或可以马上切换。
2. 检查四项持久化门禁：Client 就绪、角色目录同步、用户投影完成、Broker 登录验证。门禁绑定当前 Client 配置 hash；未完成 outbox（PENDING/RUNNING/FAILED）会撤销用户投影完成条件。
3. 失败投影先处理依赖，再通过页面选定事件及原因重放；重放只重新排队，不代表同步完成。成功以持久化状态、当前配置 hash 及无待处理事件为准。
4. Broker 验证须来自真实验签的会话证据；Admin API 同步成功不能冒充登录成功。四项齐备后启动 7 天观察期，观察未结束不能切换。
5. 观察结束且服务端重新校验门禁后，具有登录目标更新/OAuth Client 禁用等切换权限的管理员执行“切换 Keycloak”。确认 Agent 执行、部署状态和新登录均正常，不以按钮提示或 HTTP 202 当成功。
6. 成功切换后回滚窗口也是 7 天；窗口内按页面“回滚基础平台”及服务端条件恢复原提供方。超过窗口不能保证页面允许回滚，需升级为受控恢复流程。

既有环境不会因默认 issuer 设置被隐式切换；新目录默认 alias 也不等于 Client、凭据、投影、运行时已就绪。Secret 只在服务端和受控 Agent 间传递，不从页面导出。

## 当前页面与服务端限制

页面提供仅满足三项门禁时的运行时准备提示，但当前 updateSubsystem 对 Keycloak 准备/切换仍要求四项及观察期条件；不要写成“三项满足就一定能部署准备”。若被阻断，保留 request_id 由主 Agent/研发处理，不绕过门禁。本次仅改文档。

目录发布、岗位/任职和用户例外变更推进投影；快照按精确环境求值，只有 GRANTED 携带角色/权限，冲突或无授权投影空集以撤销旧角色。周期对账在活动 Client 映射内补偿绑定/任职/超管自然生效及到期，只入普通 outbox，PENDING/RUNNING 抑制重复；未建立 SYNCED 投影仍需首次接入或受控重投。排队、平台事务提交与 Keycloak 远端成功不是一回事。失败后依赖恢复和 Worker 运行是必要条件；投影失败通知是辅助，不是成功证明。

## 故障边界

固定策略 `continue_existing_platform_sessions` 允许已建立的平台浏览器会话按原超时/撤销规则继续，不允许用缓存或过期 Keycloak Token 创建新平台会话。新 Broker 登录、Client 同步及切换依赖不可用时失败关闭。使用 Keycloak 的子系统已有会话能否继续须看各自复核/JWKS 实现，不能泛化为全部系统持续可用。

关联：[接入与生命周期](subsystem-onboarding.md)、[源码证据](../../.ai/modules/operation-docs-platform-20261013.md)。
