# 停机契约（跨仓统一基线）

> 源码校准：2026-11-06。本轮只做只读源码核对：**未构建、未启动进程/容器、未发信号、未连数据库**。文件:行号取自各仓本轮 worktree 的当前文件；改实现后必须同步改这里。

本文件是 platform 与 5 个业务仓共享的停机契约，只回答五件事：预算、非法值、取消顺序、退出码、在途批次。各仓实现可以独立，语义必须一致；引用本文件时不要另写一份解释。

## 1. 契约四要素

| 要素 | 契约 | 现状与差异（含已知不一致） | 证据 |
| --- | --- | --- | --- |
| 预算来源 | 预算必须显式且有限：platform 用进程内固定常量；业务仓默认 20s，可用 `<REPO>_SHUTDOWN_DRAIN_SECONDS`（整数秒）覆盖。 | platform 四进程固定 15/20/30/20s，且明确不读任何 `*_SHUTDOWN_DRAIN_SECONDS`（改动需另立项并同步部署清单）。业务仓默认值一致（均 20s），但 env 名按仓分裂，且 Settlement api 不读该变量。 | platform `internal/shared/shutdown/manager.go:11-21`；各仓见 §2 |
| 非法值行为 | 非法值（非整数、0、负数）不得被解释成 0 预算或无限等待，必须回落到默认值或启动失败。 | **已知不一致**：data_analysis 启动期 fail-closed，非法值直接非零退出；contract_management、Settlement、customer_and_opportunity、project_management 静默（或仅 WARN）回落默认 20s。同一个配置手误在 DA 让进程起不来，在其他仓被吞掉。 | DA `internal/shutdown/group.go:33-45` + `cmd/aggregation-worker/main.go:38-42`；Settlement `cmd/worker/shutdown.go:86-98`；CRM `internal/shutdown/groups.go:34-44`；contract `internal/lifecycle/waiter.go:29-35`；PM `internal/shutdown/group.go:100-110` |
| 取消顺序 | 先停止接收新工作（HTTP/Socket/Worker 领取），再取消后台循环，最后在预算内 join；顺序反了会把在途请求/批次的收尾窗口掐掉。 | **已知差异**：platform Manager 在钩子之前先 cancel 生命周期 ctx，因此后台协程先收到取消信号，与 HTTP 排空并行推进（不是等排空结束再取消）；业务仓相反，先 HTTP 排空再取消后台。**风险点**：file-gateway 的 `file-reconciliation` 与 `local-capacity-monitor` 在 HTTP 排空前已被取消，对账批次可能在事务中途被打断，需靠下一轮对账自愈；这是数据窗口问题，不是预算问题。 | platform `internal/shared/shutdown/manager.go:176`→`:187`、`cmd/file-gateway/main.go:140,142,150`；contract `cmd/api/main.go:410`→`:422`；CRM `internal/bootstrap/app.go:586`→`:589`→`:595`；PM `cmd/api/main.go:296`→`:298`；DA `cmd/dashboard-api/main.go:85`→`:90`→`:93` |
| 退出码 | 正常停机 0；预算耗尽、钩子失败、组件未退出必须非零，不能把“没停下来”写成 0。SIGTERM 触发的非零只表示本次没在预算内收干净，**不等于部署失败**。 | **已知差异**：Settlement api 与 contract API 的 HTTP 排空错误只记日志（contract 用 `_ =` 丢弃），退出码只由后台 join 决定；CRM 的非 `StuckError` 关闭错误同样只记日志。即“HTTP 在途没排空”在三仓不会变成非零。 | platform `cmd/api/main.go:113`、`cmd/file-gateway/main.go:183`、`cmd/subsystem-provisioner/main.go:93`、`internal/bootstrap/worker.go:139-142`；contract `internal/lifecycle/waiter.go:96-106` + `cmd/api/main.go:426-427`、`cmd/worker/main.go:179-180`；Settlement `cmd/worker/main.go:362-366`；DA `internal/shutdown/group.go:106-135` + `cmd/dashboard-api/main.go:93-97`；CRM `internal/shutdown/groups.go:78` + `cmd/crm-server/main.go:69-82`；PM `internal/shutdown/group.go:71-85` + `cmd/api/main.go:298-300` |

## 2. 预算与环境变量清单

| 仓 | 长驻进程 | 预算 | 覆盖变量 | 位置 |
| --- | --- | --- | --- | --- |
| platform | api | 15s（HTTP 停止接收 + 排空） | 无，固定常量 | `cmd/api/main.go:21`、`:74` |
| platform | worker | 20s（后台循环 join） | 无，固定常量 | `internal/bootstrap/worker.go:54` |
| platform | file-gateway | 30s（其中 HTTP 排空 10s） | 无，固定常量 | `cmd/file-gateway/main.go:31,34`、`:133` |
| platform | subsystem-provisioner | 20s（Socket 停收 + 在途部署） | 无，固定常量 | `cmd/subsystem-provisioner/main.go:26`、`:62` |
| contract_management | api、worker | 20s | `CONTRACT_SHUTDOWN_DRAIN_SECONDS` | `internal/lifecycle/waiter.go:21`；`cmd/api/main.go:56`；`cmd/worker/main.go:50` |
| Settlement | worker | 20s | `SETTLEMENT_SHUTDOWN_DRAIN_SECONDS` | `cmd/worker/shutdown.go:15,86` |
| Settlement | api | 20s（仅 HTTP 排空） | 无，硬编码 | `cmd/api/main.go:169` |
| data_analysis | dashboard-api、aggregation-worker、alert-worker | 20s | `DATA_ANALYSIS_SHUTDOWN_DRAIN_SECONDS` | `internal/shutdown/group.go:23,27`；`cmd/dashboard-api/main.go:24`；`cmd/aggregation-worker/main.go:38`；`cmd/alert-worker/main.go:35` |
| customer_and_opportunity | crm-server、portal-server | 20s（内部 HTTP 10s + 后台 join 10s） | `CUSTOMER_OPPORTUNITY_SHUTDOWN_DRAIN_SECONDS` | `internal/shutdown/groups.go:20,23,26,29,34` |
| project_management | api、background（worker/sla-notifier 共用入口） | 20s | `PROJECT_MANAGEMENT_SHUTDOWN_DRAIN_SECONDS` | `internal/shutdown/group.go:20,22,100`；`cmd/api/main.go:296-298`；`internal/background/entrypoint.go:130-137` |

platform 的四个值不是“默认值”，是当前硬编码事实（`internal/shared/shutdown/manager.go:11-21` 已声明该立场）；业务仓的 env 只接受整数秒，PM 额外接受浮点（`internal/shutdown/group.go:105`），属于实现细节，不要写进部署模板。

## 3. 在途批次语义

- 契约：ctx 取消后仍必须落库的批次，一律用 `context.WithoutCancel(ctx)` + 2–5s 超时写到终态，避免行停在 CLAIMED 等租约过期。
- contract_management：**已实现**（源码层面，未运行验证）。`PersistContext`（`internal/lifecycle/waiter.go:134`）与 `DrainContext`（`:145`）上限 5s（`:23`）；使用点 `internal/auditoutbox/dispatcher.go:92,120`、`internal/integration/notification/dispatcher.go:146,186`、`internal/integration/crm/link.go:153,163`。
- customer_and_opportunity：**仅失败/重试回写**用 `WithoutCancel`（`internal/middleware/request_audit.go:120`、`internal/shared/requestaudit/dispatcher.go:226`，均 2s），业务批次落库没有同一语义 → **待统一**。
- data_analysis：**仍用进程 ctx**，取消会直接中断在途轮次（`internal/alertworker/loop.go:16`、`internal/aggregation/worker.go:66` 把信号 ctx 直接交给 `RunOnce`）；只有轮次汇总用 `WithoutCancel`（`internal/aggregation/aggregation.go:205`）→ **待统一**。
- platform：worker 的后台循环靠租约/Job 表回收，取消后只点名未退出者并非零退出（`internal/bootstrap/worker.go:139-142`）；api/file-gateway 没有“已认领批次”语义。

## 4. 部署边界

- 所有预算都必须小于编排的 `stop_grace_period: 60s`（`deploy/production/docker-compose.yml:42` 等 49 处）；超过宽限期由运行时 SIGKILL，退出码 137，此后不再有 drain 摘要，也不能据此判断预算是否合理。
- SIGTERM 路径的非零退出码应读成“本次停机未收干净”，处置是查日志点名与重启，不是回滚发布；发布脚本口径见 `deploy/production/README.md`「发布行为」。

## 5. 未決事項

1. **统一 env 名**：`<REPO>_SHUTDOWN_DRAIN_SECONDS` 已是事实约定，但 platform 四进程固定常量、Settlement api 读不到该变量。下一步二选一：给 platform 加同名变量（同时改部署清单与本文件），或在契约里写死“platform 不支持覆盖”。当前按后者记录（`internal/shared/shutdown/manager.go:19-21`）。
2. **非法值统一 fail-closed**：DA 已 fail-closed，其余四仓静默回落。统一会改变现有部署的启动行为（配置错误从“能启动”变“起不来”），需要单独发布窗口与清单核对，本轮不做。
3. **是否抽共享包：建议不抽**。六仓独立部署、Go module 互不引用，CRM 包注释已明确“不跨仓库引用平台实现”（`internal/shutdown/groups.go:1-5`）；抽包会把停机预算变成跨仓版本耦合。维持 N 份实现 + 各仓测试 + 本文件作为契约与门禁，与 `docs/async-processing-conventions.md:5` 的“跨仓库稳定协议，不是要求各子系统导入同一个业务实现包”一致。

演练步骤与记录表见同目录 [shutdown-drill.md](./shutdown-drill.md)。
