# Current Task

## Focused follow-up: subsystem discovery excludes adopted applications (2026-09-17)

- Fixed subsystem discovery to compare Docker candidates against the tenant's active application registry instead of the current user's permission-filtered portal list.
- Registration identity is the normalized application code. Once an application is active in any environment, Docker candidates for that application no longer appear as unregistered merely because their environment label differs.
- Inactive/draft registrations remain discoverable so an incomplete adoption can still be retried through the normal onboarding path.
- Added service and HTTP regression coverage for normalization, deduplication, cross-environment filtering and case-insensitive matching.
- Focused application-registry tests, full `go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` pass. The local API/provisioner image was rebuilt and both services are healthy; browser verification is blocked only by the expected session invalidation caused by the API restart.

## Focused follow-up: settlement portal card visibility (2026-09-16)

- Fixed portal application visibility so a directly assigned, active tenant-scoped `platform-super-admin` receives the same cross-application visibility already provided by the effective-authorization service.
- Fixed directory-only subsystem adoption so the adopting operator receives the application-owned initial administrator role after the deployment agent publishes the catalog; the recipient and completion time are persisted atomically for idempotent retry.
- Local runtime verification passed: the portal displays the settlement card and opens `/settlement/dashboard`; the settlement dashboard loads successfully.
- Focused application-registry/bootstrap tests and the full `go test ./...` regression pass; `git diff --check` passes. Changes are local/uncommitted; the local API image was rebuilt for browser verification.

## Focused follow-up: position-template duplicate inspection (2026-09-16)

- Added a one-click duplicate check to “将模板应用到岗位”. It analyzes all selected templates, not only the current page, and reports the repeated application role, scope and source templates.
- Duplicate identity is application + role + scope; a duplicate is reported only when effective periods overlap. Disabled roles, disjoint periods and different environment scopes do not produce false positives.
- Template or position selection changes invalidate the prior result. The check is read-only and never deletes templates or mutates assignments.
- Focused tests pass 7/7; the full frontend suite passes 518/518 and the production build passes. Changes are local/uncommitted and not deployed.

## Focused follow-up: project workflow production integration (2026-09-14)

- Registered the contract approved-state reader as a machine binding for project management.
- Added project file upload/bind scopes and production environment wiring.
- Added `project-sla-notifier` to production Compose and the project image build targets.
- `go test ./...` passed with an isolated GOCACHE; production Compose config validation passed.
- Deployment has not been performed.

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
