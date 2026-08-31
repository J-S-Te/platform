# Current Task

## 目标

修复 Keycloak 用户已禁用但基础平台账号列表仍显示启用的问题。

## 本轮完成

- 基础平台账号列表按关联用户 `identity_id` 回读 Keycloak 用户 `enabled` 状态，覆盖本地密码账号对应的 Keycloak 投影用户。
- 回读使用平台 `identity_id`，并校验 Keycloak 用户的 `tenant_id` 属性，防止跨租户误匹配。
- Keycloak 用户已禁用时，平台账号列表返回 `DISABLED`，前端现有状态展示自动显示“停用”。
- 没有关联用户的服务账号不调用 Keycloak；有用户关联的本地密码账号也必须与对应 Keycloak 投影状态一致。
- Keycloak 查询失败会显式返回错误，不会错误显示为启用。
- 增加 Keycloak 状态回读、租户不匹配和账号列表映射回归测试。

## 已执行验证

- 相关 Go 测试：通过。
- 基础平台 `go test ./...`：通过。
- 基础平台 `go vet ./...`：通过。
- `git diff --check`：通过。

## 部署提示

需要使用包含本次代码的基础平台 API 镜像/进程重新部署；Keycloak 管理凭据和用户 `tenant_id` 属性必须已正确配置。状态回读发生在账号列表请求中，首次刷新账号页面即可看到最新状态。
