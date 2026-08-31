# Basic Platform Context

基础平台负责租户、身份、登录账号、组织岗位、授权和 Keycloak 接入。`iam_user` 与 `iam_account` 是平台身份事实源；Keycloak 是外部身份与授权投影目标。Keycloak 管理客户端位于 `internal/platform/keycloakauthorization/infrastructure`，身份管理应用位于 `internal/platform/identity/application`。

关键约束：所有查询按租户隔离；本地密码账号只使用平台状态；Keycloak/FEDERATED 账号需要在 Keycloak 管理状态可用时核对外部用户状态；Keycloak 凭据不得进入日志或响应。
