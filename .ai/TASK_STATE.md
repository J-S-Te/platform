# Current Task

## 目标

修复临时账号到期、账号处于锁定窗口时，基础平台仍显示启用且 Keycloak 未同步禁用的问题。

## 本轮完成

- 账号列表按有效状态展示：本地状态为 `ACTIVE` 但已到期时显示“已失效”，锁定窗口未结束时显示“已锁定”。
- Keycloak 授权投影仅在关联用户存在至少一个 `ACTIVE`、未到期且未锁定的登录账号时保持启用。
- 投影表持久化最近一次 `user_enabled` 状态；Worker 每轮比对当前账户有效性与投影状态，将差异通过既有 Outbox 投递 `IDENTITY_CHANGED` 事件。
- 临时账号自然到期、锁定开始或结束后，均能在下一个 Worker 轮询周期禁用或恢复 Keycloak；存在其他有效登录账号时不会错误禁用同一用户。

## 涉及文件

- `internal/platform/keycloakauthorization/infrastructure/projection_source_gorm.go`
- `internal/platform/keycloakauthorization/infrastructure/projection_store_gorm.go`
- `internal/platform/keycloakauthorization/infrastructure/account_eligibility_reconciler.go`
- `internal/platform/keycloakauthorization/worker/worker.go`
- `internal/bootstrap/worker.go`
- `migrations/000102_add_keycloak_projection_user_enabled.sql`
- `frontend/src/modules/platform/iam/utils/iamPresentation.js`
- `frontend/src/modules/platform/iam/components/IamSettingsModule.vue`

## 已执行验证

- Keycloak 投影、Worker、身份应用和启动装配相关 Go 测试：通过。
- 前端账号有效状态测试：通过。
- 前端完整测试（430 项）与生产构建：通过。
- 平台与前端的 `git diff --check`：通过。

## 部署提示

先执行 `000102_add_keycloak_projection_user_enabled.sql`，再部署并重启包含 Keycloak Worker 的基础平台服务。线上现有已过期或锁定账号会在 Worker 的下一个轮询周期完成投影对账。
