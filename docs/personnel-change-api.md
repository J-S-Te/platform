# 人员异动中心操作与 API

> 源码核验日期：2026-10-22。核验覆盖页面、API、会话权限、状态机、事务仓储、迁移及 API/Worker 装配；未进行新的动态验收，不代表当前部署已启用全部依赖。

## 角色与入口

入口为平台控制台“人员异动中心”。查询、预览、交接列表需要 `platform:user:read`；新建、提交、取消需要 `platform:user:update`；状态迁移（含审批、排期、立即执行）和完成交接项同时需要 `platform:user:update`、`platform:approval:process`。读取人员、组织、岗位、任职目录还受各自读取权限约束。不能用 CRM 的业务角色名称替代这些平台权限。

租户、操作者和是否平台超级管理员由服务端会话确定。普通审批人必须同时不同于提交人和异动本人，否则应用服务以 `ErrSelfApprovalForbidden` 拒绝，HTTP 返回 `403 / AUTH_FORBIDDEN`。仅服务端认定的 `platform-super-admin` 可填写非空 `exception_reason` 使用例外；例外只豁免自审约束，不豁免交接、租户、版本或有效期门禁。例外原因、状态轨迹与审计 outbox 在同一事务提交（迁移 `000126`），缺少审计端或事务 outbox 能力时拒绝。Worker 以固定 event ID 补投审计，外部投递失败保留 PENDING 并退避，不把已提交审批改报失败。

## 条件、步骤与成功判断

1. 点击新建，选择晋升 `PROMOTION`、降职 `DEMOTION`、调岗 `TRANSFER`、离职 `TERMINATION` 或复职 `REHIRE`。
2. 晋升、降职、调岗、离职选择该人员当前活动的原任职；非离职还需选择活动的目标组织及其岗位。复职选择停用人员，后端要求可恢复的已初始化本地账号。
3. 填写原因和生效日期，先做权限影响预览，再保存。页面日期按北京时间零点提交；“立即生效”仅把生效时间设为当前时刻，不代表跳过审批。
4. 普通操作者保存后为 `DRAFT`，点击提交进入 `PENDING_APPROVAL`。服务端认定的 `platform-super-admin` 新建非离职异动直接为 `SCHEDULED`，此路径不要求审批凭据；离职不享受此捷径。
5. 审批通过时填写非空审批依据；驳回时同一字段承载驳回原因。这是留痕文本，不是经验证的外部审批单、签名或附件。非离职通过后排期；离职通过后必须进入 `PENDING_HANDOVER`。
6. 离职读取交接清单，选择同租户活动且不是离职本人的承接人，逐项转移，再申请排期。无批次的本地人工项以 `COMPLETED` 结清；带 `batch_id` 的编排项必须依据真实回执到达 `TRANSFERRED/SKIPPED`，人工改为 `COMPLETED` 不能代替远端结果。清单至少一项且无未结项；协调器启用时还要核验跨系统批次、无阻断系统及全部远端项完成，必要时结束批次并解除冻结。未给交接凭据时服务端生成 `HANDOVER-` 编号，手填时要求此前缀；凭据仅为审计索引。
7. 已排期异动由 Worker 到期执行，或有迁移权限的管理员点击立即执行。终态为 `EXECUTED`；`REJECTED`、`CANCELLED` 不执行。

成功不能只看保存提示：重新读取异动单、任职、用户/账号状态和服务端应用授权；排期不是执行完成，授权预览也不是未来时点的部署验收。

## 执行的真实副作用

- 晋升、降职、调岗：事务中关闭原任职、创建目标任职，沿用原主任职/继承属性，并撤销平台账号会话及推进授权投影。
- 离职：停用全部任职、关联账号和用户，清空主组织并标记离职，撤销平台会话。保留人员、异动历史及原用户个人直接授权记录，不物理删除业务数据。因用户停用失效的个人例外应另行复核，不能假设复职时永不恢复。
- 复职：恢复符合条件的一个本地人工账号及人员，创建主任职、开启继承、清除账号锁定/到期条件，复用原密码摘要并要求改密；不生成新的初始密码，旧密码未知时走账号重置。
- 审批进入离职交接时，仓储保证有平台“人员责任”项；协调器已注入时随后创建跨系统批次，先冻结再发现责任。该步骤失败可能已持久化 `PENDING_HANDOVER`，先重读单据，再通过交接重试恢复，不重复审批。
- 完成接口对无批次项只做本地签认；对带批次项设置承接人并调用协调器派发，远端按稳定幂等键、版本和租约执行真实转移。接口响应仍须核对返回状态，不能把请求受理视为转移完成。
- 页面逐项转移不是跨系统全局事务，已完成项不会因后续失败回滚；取消先核实在途并解除冻结，已转移资源保留。离职排期和执行（包括到期 Worker）均重检交接；核验失败时不推进。
- 执行后的站内信是提交后尽力通知，通知失败不回滚异动；不能以收件箱有无通知判断执行成败。

## API 与失败处理

路径前缀 `/api/v1`：

| 操作 | 方法及路径 |
| --- | --- |
| 查询 / 新建 | `GET /personnel-changes`、`POST /personnel-changes` |
| 新建前预览 / 已有单预览 | `POST /personnel-changes/preview`、`GET /personnel-changes/{id}/preview` |
| 提交 / 取消 | `POST /personnel-changes/{id}/submit`、`POST /personnel-changes/{id}/cancel` |
| 状态迁移 | `POST /personnel-changes/{id}/transition`，字段 `to_status`、`approval_reference`、`exception_reason`、`immediate` |
| 交接清单 / 完成 | `GET /personnel-changes/{id}/handover-items`、`POST /personnel-changes/{id}/handover-items/{item_id}/complete`，字段 `target_user_id` |
| 重试冻结与责任发现 | `POST /personnel-changes/{id}/handover/retry`；仅离职且处于 `PENDING_HANDOVER`，不改写已完成远端回执 |

状态冲突、版本变化、未到生效时间或清单未完成时，先刷新单据并核实条件，不绕过状态机；403 先核实两项权限以及审批人是否为提交人或异动本人；组织岗位停用或原任职已改变时重新预览；交接依赖/数据库故障交运维恢复。Worker 未运行时已排期请求不会靠浏览器自动执行。

源码依据：[`状态迁移与交接门禁`](../internal/platform/identity/application/personnel_change.go)、[`交接检查器`](../internal/platform/identity/infrastructure/personnel_handover_gorm_checker.go)、[`事务审计 outbox`](../internal/platform/identity/infrastructure/personnel_change_gorm_repository.go)、[`Worker 装配`](../internal/bootstrap/worker.go)；跨系统契约见[人员交接协议](../../.ai/modules/personnel-handover-protocol.md)。2026-10-13 的原核验保留为历史证据；本次自审 HTTP 映射已通过本地 Go 回归，未运行真实数据库、浏览器审批/交接、Worker 进程或部署验收。
