# Basic Platform Context

基础平台负责租户、身份、登录账号、组织岗位、授权和 Keycloak 接入。`iam_user` 与 `iam_account` 是平台身份事实源；Keycloak 是外部身份与授权投影目标。Keycloak 管理客户端位于 `internal/platform/keycloakauthorization/infrastructure`，身份管理应用位于 `internal/platform/identity/application`。

关键约束：所有查询按租户隔离；关联平台用户的账号会在 Keycloak 管理状态可用时核对外部用户状态；Keycloak 禁用状态会以单向、幂等补偿方式持久化为平台用户及关联账号的 `DISABLED`，外部 `ACTIVE` 不得反向启用平台禁用状态；Keycloak 凭据不得进入日志或响应。
