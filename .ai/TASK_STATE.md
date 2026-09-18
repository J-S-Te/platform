# Current Task

## Focused follow-up: personnel change modal actions (2026-09-18)

- Fixed the personnel-change create modal so its form body is the only scrolling region; the cancel and save action bar remains visible within the modal at all viewport heights.
- Added a source-contract regression assertion for the flex layout, constrained body and non-scrolling action bar.
- Frontend `npm test` passes 540 tests and `npm run build` passes; the build retains the existing large-chunk warning only.

## Focused follow-up: personnel-change validation and approval/handover policy (2026-09-18)

- Personnel-change creation now validates the selected user, active source membership ownership, and active target organization/position combination before persistence; execution retains the same checks as a second safety boundary.
- Termination requires an active source membership and always starts as a draft, including for platform super administrators. It must pass approval, enter handover, provide a `HANDOVER-*` reference, and pass the handover checker before scheduling.
- Regular administrators create drafts and submit them for approval. Only a server-verified `platform-super-admin` role may directly schedule non-termination changes; browser payloads can no longer choose whether approval is required.
- Rehire accepts no source membership, requires an active target assignment, and only accepts a disabled user. Termination records `TERMINATED`; successful rehire restores `EMPLOYED`.
- The frontend now includes disabled users for rehire selection, exposes submit/approval/handover actions, records approval or handover references in a controlled auto-closing dialog, and documents the real workflow.

## Focused follow-up: personnel change form validation (2026-09-18)

- Unified personnel-change field rules across Vue and backend validation: promotion/demotion/transfer require source membership plus target organization and position; rehire requires target organization and position; termination does not require a target assignment.
- The form now shows conditional required markers and guidance instead of marking optional termination/rehire fields as mandatory.
- Added backend lifecycle regression coverage for each change-type validation branch.
- Platform identity tests and frontend `npm test` (540 tests) plus `npm run build` pass.

## Focused follow-up: viewport-adaptive subsystem portal cards (2026-09-17)

- The desktop subsystem portal now uses the remaining `100dvh` workspace instead of a fixed 248px card minimum. One through nine visible applications map to 1×1, 2×1, 3×1, 2×2, 3×2, 4×2 or 3×3 grids with spacious/standard/compact density.
- More than nine applications use stable nine-item pagination instead of shrinking labels below the readable/clickable threshold. When authorization changes reduce the catalog, the current page is clamped automatically.
- Desktop height <=820px uses a compact header/title/card treatment. Tablet/mobile keep readable card sizes and intentionally restore normal vertical scrolling instead of clipping content.
- Added pure layout/pagination helpers and regressions for every supported card count, page bounds, viewport sizing, low-height mode and mobile fallback.
- Verification completed: 10 focused portal/layout tests pass; the full frontend suite passes 533/533; production build and diff checks pass. The unified frontend image/container was rebuilt and became healthy. The refresh script's final aggregate check still exits on the pre-existing unavailable Customer Portal route (HTTP 502), after the frontend container had already been successfully replaced.
- Browser reached the rebuilt local frontend but redirected to login because no authenticated platform session was available; protected portal visual inspection was therefore not claimed.

## Focused follow-up: local CRM deployment recovery (2026-09-17)

- Fixed controlled retry/adoption so CRM and customer portal always receive a freshly minted authorization-catalog publisher credential; ordinary restarts still preserve existing one-time secrets.
- Retry now atomically updates the catalog application ID, publisher client ID and publisher secret in the mode-0600 runtime environment, preventing stale application identities from returning OAuth 401.
- Updated the integrated CRM role-configuration hash to the current embedded catalog and replaced the retired `technical_lead` bootstrap role with `technical_director`.
- Added regression coverage for startup catalog publishers and customer/portal credential redelivery.
- Local end-to-end retry passed: the environment is `READY`, the CRM route returns `{"status":"ok"}`, and platform API, provisioner, CRM API and CRM MySQL are healthy.
- Platform `go test ./...`, `go build ./...`, `go vet ./...` and `git diff --check` pass.

## Portal display theme follow-up (2026-09-17)

- Converted the subsystem portal into a full-page dark presentation dashboard while retaining a complete light palette.
- Added accessible light/dark/system theme controls in the portal header. The explicit selection is persisted under `basic-platform.portal-theme`; first use defaults to dark and system mode reacts to live OS changes.
- Canvas particle colors, cards, header, user popover, status messages and responsive layouts now resolve through portal-scoped semantic theme variables.
- Added source-contract coverage for theme persistence, system media-query handling and dark/light CSS scopes. Focused tests and the production build pass.

## Focused follow-up: optimized portal motion restored (2026-09-17)

- Restored low-contrast Canvas particles, grid glow layers, bounded card 3D pointer tilt and staggered card entry on top of the light administrative portal.
- Particle count is capped at 48, device pixel ratio at 1.5, drawing is frame-scheduled and pauses while the page is hidden. Card tilt is limited to roughly ±3 degrees and only runs for fine hover pointers.
- Entry delay is 45ms per card and capped after the eighth card. `prefers-reduced-motion` disables Canvas, tilt, transitions and entry animation.
- Focused portal tests pass 6/6, the full frontend suite passes 531/531, production build and `git diff --check` pass. Changes remain local/uncommitted.

## Focused follow-up: platform frontend visual unification (2026-09-17)

- Unified platform design tokens in the globally imported platform stylesheet; console and IAM styles now consume semantic aliases.
- Simplified login and password-change brand panels to a solid dark layout, and rebuilt the subsystem portal as a light administrative workspace without particles, glow layers, 3D pointer effects or glass blur.
- Normalized platform typography and radii, removed generic `transition: all`, reduced `!important` usage to reduced-motion overrides, and consolidated duplicate console backdrop/search definitions.
- Added source-level UI contract tests. Focused UI tests, the full 531-test frontend suite, production build and `git diff --check` pass. The local frontend container was rebuilt and is healthy; login-page visual verification passed at desktop and narrow viewport. Changes are local/uncommitted.

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
