# 停机演练 Runbook（计划中，未执行）

> 状态：**计划**。2026-11-06 只编写步骤与记录模板；**未构建、未启动任何进程/容器、未发信号、未跑探针、未连数据库**。本文件不是验收证据，执行后必须回填实测值并另存证据。
> 契约基线：[shutdown-contract.md](./shutdown-contract.md)。预算、取消顺序、退出码有变更时先改契约，再按本文件复验。

## 1. 前置条件

- 隔离环境：独立 `COMPOSE_PROJECT_NAME`、独立网络/卷/端口，**不得**复用生产或开发共享栈；演练会 stop/kill 容器，禁止在承载业务流量的环境执行。
- 测试 DSN：每仓指向一次性测试库（演练后整库丢弃）；禁止连接业务库、禁止复用业务数据卷。
- 被测物可追溯：记录各仓 branch + commit、镜像 digest、被测二进制路径；预算变量按 §2 步骤 8–9 的用例设置并记录原值。
- 工具：`docker`、`curl`、`hey` 或 `wrk`（固定 20 req/s）、`timeout`、`jq`。
- 证据目录：`.ai/qa/shutdown-drill/<YYYYMMDD-HHMM>/`（各仓或 platform 归档均可），日志与 `docker inspect` 输出直接落盘；证据里不得出现 DSN、Token、Cookie、私钥。

## 2. 步骤

1. 逐个构建各二进制（在各自仓根，一次只传一个 main 包才会产出可执行文件）：`go build -o <输出目录>/<binary> ./cmd/<binary>`。platform 四个：`api`、`worker`、`file-gateway`、`subsystem-provisioner`；contract 两个：`api`、`worker`；Settlement 两个：`api`、`worker`；DA 三个：`dashboard-api`、`aggregation-worker`、`alert-worker`；CRM 两个：`crm-server`、`portal-server`；PM 两个：`api`、`worker`（background 入口）。
2. 起进程：优先单进程直跑（可拿真实退出码），必要时用 compose 单服务起；等待 `/readyz` 或等价就绪信号，确认启动日志无错误、无重启循环。
3. 基线探针：对只读端点（如 platform `/livez`、`/readyz`）以固定 **20 req/s** 跑 30s，确认基线无 5xx；记录 P50/P99。
4. 发信号：`kill -TERM <pid>`（容器用 `docker stop -t 60 <service>` 的等价路径由步骤 7 复验），探针持续到进程退出后才停。
5. 断言（逐条落盘，任一失败即本次不通过）：
   - 退出码：正常路径 `0`；预算耗尽或钩子失败 `1`；**不得**出现 `137`（SIGKILL）或信号未处理导致的 `143`。
   - 日志：含 drain 摘要（platform `shutdown draining started`，contract/Settlement/DA/CRM/PM 各自 drain 日志）与结束时 `drained cleanly` 或未退出组件点名（`still running` / `unfinished` / `pending goroutines`）。
   - 时长：信号到退出 ≤ 该进程预算（§3 记录表登记预算）；超预算必须走非零退出并点名。
   - 探针：无非预期 5xx；无 `ECONNRESET`（RST）。排空开始后的新连接 `ECONNREFUSED` 属预期，单独计数，不进失败判据。
   - 组件：无未退出组件；若有，记录点名清单与最后一条该组件日志。
6. 正常路径每种进程至少 3 次，观察抖动；单次异常需复跑确认是否可复现。
7. `docker stop -t 60 <service>` 复验：重复步骤 3–5；用 `docker events` 确认只出现 `stop`/`die`，无 `kill` 事件；`docker inspect -f '{{.State.ExitCode}} {{.State.OOMKilled}}'` 不含 `137`/`true`。
8. 超预算路径：把某仓预算临时设为 `1`（如 `CONTRACT_SHUTDOWN_DRAIN_SECONDS=1`）+ 制造慢组件（隔离库锁行或注入 sleep），确认非零退出、日志点名组件；恢复原预算并记录已恢复。
9. 非法值路径：设 `0`、`-1`、`abc`，按契约 §1「非法值行为」断言：DA 必须启动失败非零退出，其余仓回落默认 20s 并（Settlement）打 WARN；把实际行为记入记录表备注。
10. 演练结束：恢复预算变量、销毁隔离栈与测试库，证据目录只保留脱敏日志与命令输出。

## 3. 记录表模板

| 进程 | 预算 | 实测退出秒数 | 退出码 | 未退出组件 | 探针 5xx/RST | 证据路径 |
| --- | --- | --- | --- | --- | --- | --- |
| platform-api | 15s | 待填 | 待填 | 待填 | 待填 | 待填 |
| platform-worker | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| platform-file-gateway | 30s | 待填 | 待填 | 待填 | 待填 | 待填 |
| platform-subsystem-provisioner | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| contract-api | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| contract-worker | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| settlement-api | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| settlement-worker | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| da-dashboard-api | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| da-aggregation-worker | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| da-alert-worker | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| crm-server | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| portal-server | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| project-api | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |
| project-background | 20s | 待填 | 待填 | 待填 | 待填 | 待填 |

## 4. 通过判据与已知风险

- 通过 = 全部进程退出码符合契约 + 无 SIGKILL + 无未退出组件 + 探针无非预期 5xx/RST + 实测时长 ≤ 预算。
- 必须单独观察 file-gateway：其后台对账协程在 HTTP 排空之前被取消（见契约 §1 取消顺序），演练时要确认对账行没有停在中间态、下一轮能自愈；不能只凭退出码 0 判通过。
- 编排宽限为 `stop_grace_period: 60s`（`deploy/production/docker-compose.yml:42` 等），所有预算必须显著小于它；超出后被 SIGKILL 的样本不计入“通过”，只作为预算不合理的证据。
