# 统一生产编排与裁剪

统一交付检查、安装阶段、状态查询与恢复命令见 [DELIVERY_WORKFLOW.md](DELIVERY_WORKFLOW.md)。

升级关键配置保留规则：已有 `.env`、`runtime/*.env` 的密码、密钥和初始管理员记录保持原值；缺失或空白材料不自动补造。首次创建配置时才生成凭据，缺失既有签名密钥时要求恢复。详见 [初始凭据与更新边界](OFFLINE_RUNBOOK.md#63-安全查看初始管理员凭据)。

> 2026-10-22 当前工作区源码核对；未运行部署或动态验收。发行完整性、结构检查、安装会话与授权分别核验，历史记录保持原日期。


> 构建、复制、空白机安装、配置、分阶段部署、子系统接入和故障处理的本目录详细操作手册是
> [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md)。本文件保留架构、生产编排和 CI/CD 参考，不应作为另一套离线执行流程。
> 服务器验收逐项记录使用 [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md)。

生产只使用 `docker-compose.yml`。公共平台必选；七个可选块以 `# BEGIN SUBSYSTEM: contract|project|customer|portal|settlement|data-analysis|monitoring` 和对应 END 标记划分。整块注释即可排除该系统，无需修改 profiles 或额外启用列表。部署脚本和平台 Agent 都以 Compose 解析出的服务为准，拒绝发布被注释模块。运行配置缺失不阻止解析，但接入完成前不能启动业务容器。

正式离线交付使用根中文入口的可信发布验签与分阶段安装，示例见[批次操作说明](../../../操作说明/README.md)；固定批次摘要不会随源码更新而改变。当前组件登记支持内置目录V2和遗留V1桥接，JWS/快照协议仍为1。V1的运行批准发布与V2普通镜像更新有不同前提，见[商业授权总览](../../../sharedDocs/09_商业授权/README.md)。

生产清单当前为CRM9、合同2、项目2、结算2、BI3、Portal最多3个消费者；项目第二进程是SLA notifier，门户邀请补偿是条件组件。Metabase属于另行核验的基础设施，不发业务ACK。验收依清单和实际启用集合，不能只核对API。

2026-10-23 项目后台改为独立 Temporal Runtime：三类Schedule调度SLA、派生事件与通知，人工审批/状态事务仍由MySQL处理。新发行模板显式temporal，Build由完整不可变镜像摘要派生。首次bootstrap健康允许登记，随后仍须真实许可、当前自然ACK、显式版本提升及execution验收。

项目更新、恢复、停用和移除统一经过 `bin/project-background.sh` 保护：先备份并保存私有状态、pause/drain，再迁移替换；新进程bootstrap后ensure/resume/verify。排空失败保留旧进程和候选，人工暂停不被恢复操作擅自解除。首次失败没有已保存旧镜像时拒绝自动回滚；修正条件后按原受控入口续跑。不得手改 `runtime/project-background/operation.json`、提前切镜像或以API健康代替后台执行验收。

调度参数与镜像同时变更时，项目私有操作记录保存旧进程实际使用的七项非秘密参数。回滚在旧容器重建前原子恢复 `runtime/project.env` 中这七项，保留凭据及其他字段，再以 restore 验证旧 Build/规格。旧参数缺失、越界、身份不符或运行文件不安全时停止恢复并保留候选；不回滚整份运行配置。未知旧控制协议须人工核对，不能绕过校验。生产仍只有两个项目消费者，四系统合计16个。

已有部署可用 `docker compose --env-file .env --env-file .release.env -f docker-compose.yml exec -T project-sla-notifier /usr/local/bin/project-background-control --action inspect` 只读诊断。旧自定义Compose由安装器保留，新模板写入 `.dist`；升级前须合并后台Build、Temporal依赖、bootstrap探针和停止等待窗口。真实签名许可/服务器验收不包含在本轮本地与隔离测试中。

```bash
docker compose --env-file .env --env-file .release.env -f docker-compose.yml config --services
sudo ./bin/deploy.sh start
sudo ./bin/deploy.sh resume customer-opportunity
sudo ./bin/deploy.sh disable contract
docker ps -a
```

修复、移除与清理入口在 `bin/lifecycle.sh`（**不由 `deploy.sh` 转发**），用 `sudo bash ./bin/lifecycle.sh help` 查看；离线部署的逐步清单见 [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md)。

`start` 分阶段启动平台密钥初始化、公共数据库、迁移、Keycloak、Temporal、File Gateway、Docker socket proxy、部署 Agent、平台与前端，再处理已接入且保留的业务系统。`resume` 恢复单系统并执行备份、迁移、目录同步和有界健康等待；首次接入仍由基础平台完成。`disable` 只停止指定模块容器，保留卷和密钥；注释本身不会停止旧容器，状态检查会列出差异。

`contract-mysql` 保留原名和卷，已移到公共区供 Temporal 使用；停用合同不停止该共享数据库。门户邀请补偿依赖 CRM，CRM 未启用或未接入时跳过此 Worker，门户主体仍可启动。

HTTP、HTTPS 和会话排空统一由同一 frontend 服务及 `runtime/public-tls/` 受管目录处理，不再加载生产 frontend/HTTPS/drain/observability 覆盖文件。升级资产使用 `bin/install-assets.sh`，保留当前 `docker-compose.yml`，将新模板写到 `.dist` 供合并；不要覆盖用户的注释范围。

### 编排文件的权威关系（BIZ-XWIRE-08）

- `docker-compose.yml` 是**唯一权威编排**，也是离线资产包唯一发布的编排文件：`bin/deploy-service.sh`、`bin/lifecycle.sh`、`bin/publish-runtime-approvals.py` 以及 provisioner 的 `SUBSYSTEM_PRODUCTION_COMPOSE_FILE` 都指向它。
- `compose.yaml` 是**不得用于部署**的评审参考副本：除注释/日志/资源上限/发布 profile 这类非关键键外，商业许可接线（每个消费者的 `runtime/license-*.env`、`license-state-*:/var/lib/commercial-license` 状态卷、`LICENSE_RUNTIME_STATE_DIRECTORY`）与健康检查（`test`/`interval`/`timeout`/`retries`）都与权威一致，缺一项即门禁失败；它不随资产包发布。
- `compose.observability.yaml` 是被权威编排 `monitoring` 子系统取代的历史覆盖示例，任何脚本都不引用；再用 `-f` 叠加只会制造同名服务的第二份定义。
- 改编排只改 `docker-compose.yml`，需要时再同步 `compose.yaml`。`tests/test-compose-orchestration-consistency.py`（CI 已登记）会校验三份文件的权威标记、两份文件的服务/卷集合、同名服务的镜像/端口/网络/environment 无矛盾、许可接线与健康检查无缺失（常驻服务必须有探针、一次性任务必须没有、PID1 探针必须指向真实 Worker 二进制）、`COMMERCIAL_LICENSE_*` 不得写进编排/模板/清单、旧部署根 `/opt/basic-platform` 不复现、参考副本不把 provisioner 指回 `compose.yaml`；出现新漂移直接失败。

### 已知差异（参考副本有意保留）

`compose.yaml` 是评审对照样本，不是等价部署样本。下列差异**有意保留**：评审不要要求逐键对齐，门禁也刻意不检查它们；照抄参考副本会得到与权威不同的解析结果。

- `profiles`：参考副本给 23 个服务标了发布/迁移 profile（`release`、`project-release`、`customer`、`customer-release`、`settlement-release`、`data-analysis-release`），方便按子系统单独解析。权威编排靠 `# BEGIN/END SUBSYSTEM` 注释块加 `bin/deploy-service.sh` 的显式服务名控制启动范围，不使用 profile。因此按参考副本执行 `docker compose up`（未启用对应 profile 时）不会启动这些服务，启动范围与权威不同，**不能**用参考副本核对发布范围。
- `portal-migrate`：参考副本经 `*portal-service` 锚点继承 `portal-reports-data:/app/data/portal-reports`；权威编排的 `portal-migrate` 只跑 schema 迁移，不挂报表卷。
- `settlement-migrate`、`settlement-catalog-sync`：参考副本经 `*settlement-service` 锚点继承 `jwt-ed25519-public.pem` 只读卷；权威编排这两个一次性任务不挂该卷。
- `env_file` 写法：参考副本用 `- runtime/*.env` 字符串简写，权威编排用 `- path: ... / required: false` 映射写法。映射写法在 runtime 文件尚未初始化时不阻断解析，字符串简写会直接报错；只有 `docker-compose.yml` 的那份写法具备部署语义。
- 锚点与非关键资源键：`x-*-service` 锚点/`<<:` 合并、`logging`、`cpus`、`mem_limit`、`pids_limit`、`stop_grace_period` 只在权威编排声明。

新增差异前先判断它属于"非关键键"还是"会改变部署解析结果"：后者一律改 `docker-compose.yml`，再同步参考副本，并让门禁覆盖到。

### 健康检查覆盖范围（O6：已补齐常驻服务，一次性任务按语义不补）

本轮已把权威编排里的常驻服务全部补上 healthcheck（`docker-compose.yml` 与参考副本 `compose.yaml` 逐键一致，门禁双向比对）。判定规则是“该服务被 `depends_on: condition: service_healthy` 依赖或本身常驻”，短命任务不加探针。

**已补齐（11 个常驻 Worker，PID1 可执行文件探针）**：`customer-opportunity-alert-worker`、`customer-owner-notification-worker`、`customer-presale-alert-worker`、`customer-presale-assignment-notification-worker`、`customer-presale-progress-notification-worker`、`customer-notification-delivery-worker`、`customer-presale-worker`、`portal-invite-compensation-worker`、`portal-report-worker`、`data-analysis-aggregation-worker`、`data-analysis-alert-worker`。

探针判据与既有的 `platform-worker`、`customer-contract-transfer-worker` 一致：

```yaml
healthcheck:
  test: ["CMD-SHELL", "test \"$$(readlink /proc/1/exe)\" = /app/<binary>"]
  interval: 5s
  timeout: 2s
  retries: 12
  start_period: 5s
```

- 期望路径 = 镜像 `WORKDIR`（`/app`）+ 容器实际运行的二进制：覆盖 `command` 的服务取该命令的文件名；`data-analysis-aggregation-worker` / `data-analysis-alert-worker` 未覆盖 `command`，路径来自镜像 `ENTRYPOINT`（`data_analysis/Dockerfile` 的 `aggregation-worker` → `/app/aggregation-worker`、`alert-worker` → `/app/alert-worker`）。
- 探针只回答“PID1 仍是该 Worker 而不是被 shell、包装脚本或重启循环替换”，不新增端口、不对外暴露任何端点，也不把业务就绪伪装成进程存在。逐服务的期望二进制登记在 `tests/test-compose-orchestration-consistency.py` 的 `PID1_EXPECTED`，写错路径或删键都会让门禁失败。

**按语义不补（12 个一次性任务）**：`platform-key-init`、`platform-migrate`、`subsystem-provisioner-socket-init`、`contract-migrate`、`contract-number-audit`、`project-migrate`、`customer-migrate`、`portal-migrate`、`settlement-migrate`、`settlement-catalog-sync`、`data-analysis-migrate`、`data-analysis-metabase-init`。

理由不是“省事”，而是探针会改坏发布门禁：这些服务 `restart: "no"`、跑完即退出，退出后 Docker 只会把 healthcheck 记成 `unhealthy`，`docker compose up --wait` 因此把“成功完成”判成启动失败。它们的一次性语义由部署脚本按退出码校验（见 `bin/deploy-service.sh`），没有任何服务用 `service_healthy` 依赖它们。门禁 `test_one_shot_services_declare_no_healthcheck` 固定了这条结论。

**行为影响与仍未验证的部分**：healthcheck 就绪后，`depends_on: service_healthy` 的等待条件与发布脚本 `--wait` 的判定从“容器在运行”变成“探针通过”。本轮只做源码与配置解析级验证（`ruby -ryaml` 解析 + 门禁断言），**没有**在本机或隔离环境跑 Docker，因此发布脚本等待窗口、探针在真实镜像里的路径/权限、重启循环的观察窗口都仍需在隔离环境按 [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md) 验收，不能据本文件宣布完成。

### 许可运行环境文件的键位（O5：`COMMERCIAL_LICENSE_*` 不进模板/清单）

`COMMERCIAL_LICENSE_ENABLED` 与 `COMMERCIAL_LICENSE_COMPATIBILITY` 刻意**不出现**在 `subsystem-templates/*.env.example`、`subsystems.d/*.yaml` 和两份 Compose 的 `environment` 里，这是默认值而非缺口：

- `COMMERCIAL_LICENSE_ENABLED` 等运行态键由平台按受控接入生成到 `runtime/license-<service>.env`（`internal/platform/applicationregistry/infrastructure/runtime_license_delivery.go` 与 `local_runtime_license.go` 写入 `COMMERCIAL_LICENSE_*` 组），Compose 只负责用 `env_file` 只读引用；`runtime/` 被 `.gitignore` 忽略，不随资产包发布。把它写进模板或清单等于让“按模板重建”覆盖平台签发的运行态凭据。
- `COMMERCIAL_LICENSE_COMPATIBILITY` 是**兼容放行开关**（`license-core/consumer/consumer.go`：只有归一后等于 `true` 才放行，且仅适用于从未登记的组件），生产必须缺省——缺省即 fail-closed。任何模板、清单或 Compose 默认值都不许打开它，否则 `COMMERCIAL_LICENSE_ENABLED=false` 就能冒充“已授权”。
- 回归守卫：`tests/test-compose-orchestration-consistency.py` 的 `test_license_compatibility_switch_is_never_wired_in_production` 断言这两类键不出现在两份编排的 `environment` 里，且 7 份模板与 6 份清单都不声明它们；发布前核对项见 [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md) 的「12.4 许可运行环境文件的键位核对」。

### 检查清单（发版前手动执行）

改过生产编排、监控规则、子系统清单或 runtime 模板后，在本仓库根执行：

```bash
cd platform
python3 deploy/production/tests/test-compose-orchestration-consistency.py
python3 deploy/production/tests/test_integration_wiring.py
```

改动只需要语法自检时（不解析 YAML、不需要 ruby）：

```bash
cd platform
python3 -c "import ast,sys; [ast.parse(open(p).read()) for p in ['deploy/production/tests/test-compose-orchestration-consistency.py','deploy/production/tests/test_integration_wiring.py']]; print('SYNTAX_OK')"
```

- `test-compose-orchestration-consistency.py`：适用时机是改动 `docker-compose.yml`、`compose.yaml`、`compose.observability.yaml`、`monitoring/prometheus/*.yml`（含告警规则）、`subsystem-templates/*.env.example`、`subsystems.d/*.yaml` 之后；需要本机有 `ruby`（`ruby -ryaml` 还原锚点与合并）。它守权威标记、服务与卷集合、镜像/端口/网络/environment 无矛盾、许可接线与健康检查无缺失（含常驻服务必须有探针、一次性任务必须没有探针、PID1 探针必须指向真实 Worker 二进制）、`COMMERCIAL_LICENSE_*` 不得写进编排/模板/清单、旧部署根不复现、监控抓取与指标端口边界，以及新增的观测面门禁：`platform-api`/`settlement-api` 抓取 target 必须是已声明服务、平台 job 必须是 `Bearer` + `credentials_file`、规则文件必须存在/被 `rule_files` 引用/被只读挂载、告警表达式里的指标名必须在“代码实际输出”白名单内；`profiles`、日志、资源上限、一次性任务的额外卷不在断言范围内（见上一小节）。
- `test_integration_wiring.py`：适用时机是改动 `subsystems.d/*.yaml`、`subsystem-templates/*.env.example`、`.env.example` 或 `docker-compose.yml` 的接线键之后；只用标准库，不读 runtime 文件、不启容器、不连数据库。
- CI 状态：两项都登记在 `.github/workflows/ci-cd.yml` 的 `test` job「Validate production deployment scripts」步骤里，紧邻执行（先一致性门禁、后接线门禁），与其它生产资产检查同一 shell 段、同一失败即停语义。"CI 绿"因此可作为两项检查都跑过的证据；但 CI 只做静态解析，仍需按 [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md) 做真实部署验收。

### 监控抓取与指标端点边界

- `monitoring/prometheus/prometheus.yml` 抓取 `keycloak`、`keycloak-backup`、`file-gateway`、`contract`（`contract-api:8081`）、`platform-api`（`platform-api:8080`，平台自身 `/metrics`）与 `settlement-api`（`settlement-api:8085`）。所有 target 都是 Compose 服务名，只走 `application` 内网，不含宿主机地址。
- `/metrics` 只由网络层保护：业务服务的指标端口仅允许回环发布（平台 8080、合同 8081、结算 8085），File Gateway 8086、Prometheus 9090、node-exporter 9100 完全不发布端口；前端 nginx 只代理显式业务路径，没有 `/metrics` 路由。禁止为了抓取补 `ports:`，也不要加公网 Ingress/反代。
- contract 的 `/metrics` 不校验凭据，但只暴露聚合计数（不含租户/合同/事件 id），且与 `/healthz`、`/readyz` 同级注册、不进 `/api/v1`。它的唯一边界就是上面这条网络限制，因此“临时放开抓取端口”不是可接受的排障手段。
- 平台 `/metrics` 额外有应用层鉴权（`RequireMetricsAccess`，SEC-B4）：配置 `METRICS_ACCESS_TOKEN` 时必须带 `Authorization: Bearer`，未配置时只放行回环对端。因此平台 job 必须用部署方提供的凭据文件抓取，见下一节。结算 `/metrics` 与合同的同级实现一致，不做应用层鉴权。
- 数析（data-analysis）与项目管理（project）当前**没有** HTTP `/metrics` 端点，本编排刻意不为它们新增抓取 job（没有端点就写 job 只会得到永远 `up == 0` 的假目标）。它们的集成缺口只能通过各自的 `/readyz` 与日志观察；需要指标时先实现端点，再改本文件与门禁。

### 观测接入：指标、告警与凭据

本节是 BIZ-XEVT2-07（back-channel logout 可观测）与 BIZ-XAPI-10（集成目的地必开）落地后新增观测面的唯一入口：新增指标清单、告警清单、部署方必须提供的凭据文件，以及发布前核对项。改指标名或改抓取配置时，必须同步本文件与 `tests/test-compose-orchestration-consistency.py`（它把指标名、规则文件挂载、凭据挂载都做成了门禁）。

#### 1. 新增抓取 job 与指标清单

| 抓取 job | 端点 | 指标 | 来源（代码实际输出） |
| --- | --- | --- | --- |
| `platform-api` | `http://platform-api:8080/metrics`（Bearer） | `platform_oauth_backchannel_unregistered_rp`、`platform_oauth_backchannel_logout_no_endpoint` | `platform/internal/platform/oidc/backchannel/observability.go:16,18` |
| `platform-api` | 同上 | `platform_http_requests_total{method,route,status}`、`platform_http_errors_total{method,route,status}`、`platform_http_request_duration_seconds_bucket{method,route,status,le}`/`_sum`/`_count`、`platform_authz_failures_total`、`platform_oidc_login_failures_total`、`platform_metric_collection_errors_total` | `platform/internal/transport/http/metrics.go:62-66`（指标名）、`:261-285`（`writeHTTPMetrics`） |
| `platform-api` | 同上 | `platform_keycloak_outbox_backlog`/`_dead`、`platform_async_job_backlog`/`_dead`、`platform_oidc_backchannel_logout_backlog`/`_failed`/`_delivered` | `platform/internal/transport/http/metrics.go:364-370` |
| `platform-api` | 同上 | `platform_async_job_retries_total`、`platform_async_job_last_success_timestamp_seconds`、`platform_database_*` | `platform/internal/transport/http/metrics.go:438-440,452-455` |
| `settlement-api` | `http://settlement-api:8085/metrics` | `settlement_outbox_pending{destination}`、`settlement_integration_destination_configured{destination}`、`settlement_integration_audit_required`、`settlement_outbox_unconfigured_pending`、`settlement_outbox_pending_query_error` | `Settlement/internal/httpapi/integration_status.go:101-130`；目的地字面量 `Settlement/internal/outbox/destinations.go:13,15` 与 `integration_status.go:31` |
| `contract`（既有） | `http://contract-api:8081/metrics` | `contract_integration_outbox_dead{kind}`、`contract_integration_outbox_dead_total` | `contract_management/internal/transport/httpapi/metrics.go:54-66` |

采集语义要在排障前读懂，否则会把“没有样本”当成“一切正常”：

- back-channel 两条 gauge 在采集失败时**不输出**（不以 0 兜底）。所以 `up == 1` 但指标缺失，本身就是故障信号。
- `settlement_outbox_pending{destination}` 只在该目的地有积压行时才有样本（`Settlement/internal/outbox/worker.go:75-94` 按 destination 分组），空 outbox 没有样本是正常的——不要对它写 `absent()`，否则健康集群会被误报。样本缺失的兜底用始终输出的 `settlement_integration_audit_required`。
- 合同 `/metrics` 在计数能力缺失或查询失败时返回 503 且不输出任何样本（`contract_management/internal/transport/httpapi/metrics.go:37-44`），同样必须用 `absent()` 才能看见。
- 平台 HTTP 请求面（`platform_http_requests_total` / `platform_http_errors_total` / `platform_http_request_duration_seconds_*`）自 C1 起共用同一组标签 `{method, route, status}`，延迟指标是 **histogram**（桶上界 `0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10` 秒加 `le="+Inf"`，`+Inf` 恒等于 `_count`）。三个维度的取值域都由代码限死：`method` 白名单（`GET/POST/PUT/PATCH/DELETE/OPTIONS/HEAD`，其它记 `OTHER`）、`route` 只取 gin 注册模式（如 `/api/v1/users/:id`，未匹配的 404/405 固定 `__unmatched__`）、`status` 只分 `1xx/2xx/3xx/4xx/5xx`（畸形状态码兜底 `other`）。真实路径与查询串**永不进入标签**：那既是高基数入口，也会把 URL 里的 token/手机号复制进指标。
- 序列数有硬上限 `maxHTTPSeries = 1000`（`platform/internal/transport/http/metrics.go:80`）：超过后新组合折叠进 `{method="OTHER",route="__overflow__",status="other"}` 继续计数，请求量不丢、基数不涨。所以看到 `__overflow__` 说明进程内的 `(method, route, status)` 组合真的超过了 1000 个，要按路由表排查“动态路由”，而不是去调大上限。
- 因为带标签，**进程空闲时这三条指标没有样本**（只有 `# TYPE` 行）：零请求不是故障，不要用它们写 `absent()` 兜底。抓取可用性的兜底统一用每次抓取都无条件输出的 `platform_metric_collection_errors_total`。
- 写 PromQL 时 `status` 是**分桶枚举字符串**，必须等值：`status="5xx"` 能命中；`status=~"5.."`、`status=~"5.*"`、`status>=500` 这类“按数字区间”的写法**永远匹配不到**任何序列（空结果 = 规则静默永不触发）。这是最容易写错也最难发现的一类坑。

#### 2. 新增告警规则与清单

`rule_files` 现为四个文件，全部只读挂载进 prometheus：

| 规则文件 | 主题 | 主要告警（`for`/级别） |
| --- | --- | --- |
| `keycloak-alerts.yml`（既有） | Keycloak 与备份时间戳 | `KeycloakMetricsUnavailable` 等 |
| `file-gateway-alerts.yml`（既有） | File Gateway 容量 | `FileGatewayStorage*` 等 |
| `platform-alerts.yml`（新增） | 平台抓取、backchannel 缺口、队列 | `PlatformBackchannelUnregisteredRP`（`unregistered_rp > 0`，15m，warning）；`PlatformBackchannelNoEndpointBacklog`（`logout_no_endpoint > 0`，30m，warning）；`PlatformBackchannelNoEndpointBacklogPersistent`（同一指标 6h，critical）；`PlatformBackchannelLogoutDeliveryFailed`（`logout_failed > 0`，30m，warning）；`PlatformMetricsScrapeDown`（`up{job="platform-api"} == 0`，5m，critical）；`PlatformMetricsSamplesMissing`（`absent(platform_metric_collection_errors_total{job="platform-api"})`，15m，warning，覆盖 token 不匹配导致的 401；探测指标必须是每次抓取都无条件输出的那一条，`platform_http_*` 带标签且空闲时无样本，不能用来探测）；`PlatformHTTPErrorRateHigh`（`sum(rate(platform_http_errors_total{status="5xx"}[5m])) / sum(rate(platform_http_requests_total[5m])) > 0.05`，10m，warning，分子只统计 5xx，分母是全站请求总量）；`PlatformHTTPMeanLatencyHigh`（`_sum`/`_count` 速率的**平均值** `> 1.5`s，15m，warning；指标已是 histogram，但本轮不新增 p95 规则，p95 告警另立项）；`PlatformMetricCollectionErrors`；`PlatformKeycloakOutboxBacklog`/`_Dead`；`PlatformAsyncJobDead`/`_Stalled` |
| `integration-alerts.yml`（新增） | 结算与合同集成 | `SettlementMetricsScrapeDown`（5m，critical）；`SettlementMetricsSamplesMissing`；按 destination 的积压阈值 `SettlementAuditOutboxBacklog`（`PLATFORM_AUDIT > 200`，30m）、`SettlementNotificationOutboxBacklog`（`PLATFORM_NOTIFICATION > 500`，30m）、`SettlementTaxOutboxBacklog`（`TAX_INVOICE_COMMAND > 200`，30m）、`SettlementOutboxBacklogTotalCritical`（合计 `> 2000`，2h）；`SettlementOutboxUnconfiguredPending`（`> 0`，15m）；`SettlementAuditDestinationRequiredButUnconfigured`（`audit_required == 1` 且 `PLATFORM_AUDIT` 未配置，5m，critical）；`SettlementNotificationDestinationUnconfigured`（6h）；`SettlementOutboxPendingQueryError`；`ContractMetricsScrapeDown`（5m，critical）；`ContractMetricsUnavailable`（`absent(contract_integration_outbox_dead_total)`，10m）；`ContractIntegrationOutboxDead`（`> 0`，10m）、`_Critical`（`> 100`，1h）、`_PerEdge`（单条出边 `> 50`，30m） |

告警语义与取舍：

- backchannel `NO_ENDPOINT` 积压的 30 分钟 warning 表示“事件已生成但目标 RP 没有登记 `logout_uri`，永远不可投递”，处理方式是补登记后受控重放，不是删除 outbox 行；6 小时 critical 表示缺口跨班次未清零、RP 侧仍保留已登出会话。两者是同一指标的两个升级档，不是两个故障。
- 积压阈值按目的地分开写（审计 200 / 通知 500 / 税控 200）：合规证据时效、用户可见提醒和开票窗口的容忍度不同。调阈值只改规则文件，不改服务。
- `/readyz` 降级只在**有指标**时写规则：Settlement 的 503（审计必开但凭据缺失）恰好等价于 `settlement_integration_audit_required == 1` 且 `settlement_integration_destination_configured{destination="PLATFORM_AUDIT"} == 0`，因此可以指标化；平台、合同、数析、项目当前没有把 `/readyz` 结果导出成 gauge，本编排**不写** readyz 规则（不臆造指标名），它们只能用 `up == 0` 与样本缺失覆盖。
- 每条规则的 `annotations.runbook` 指向本文件或既有运维文档；改本节标题锚点时要同步规则文件。

#### 3. 部署方必须提供的抓取凭据文件

平台 `/metrics` 走 `RequireMetricsAccess`，跨容器抓取必须带 token。**仓库内和离线资产包里都不包含该 token**，由部署方在部署目录生成：

```bash
cd /opt/unified-identity-platform
install -d -m 700 runtime/prometheus-secrets
umask 077
openssl rand -hex 32 > runtime/prometheus-secrets/platform-metrics-token
# 把同一个值写入 .env 的 METRICS_ACCESS_TOKEN，然后重启两个消费者
docker compose --env-file .env --env-file .release.env -f docker-compose.yml up -d platform-api
docker compose --env-file .env --env-file .release.env -f docker-compose.yml up -d prometheus
```

约束与失败症状：

- 目录 `runtime/prometheus-secrets/` 被 `.gitignore` 的 `runtime/` 覆盖：不进版本库、不随资产包发布，也**不得**复制进 `monitoring/`（`install-assets.sh` 会整体替换 `monitoring/`）。
- prometheus 只按 `credentials_file: /etc/prometheus/secrets/platform-metrics-token` 读取，令牌目录以只读方式挂载（`./runtime/prometheus-secrets:/etc/prometheus/secrets:ro`）。**不要在 `prometheus.yml` 或编排里内联 `credentials`/`bearer_token`**：门禁会失败。
- 文件缺失时 prometheus **启动即失败**（日志出现 `unable to read authorization credentials file`）。这是有意的 fail-closed：静默 401 会让 backchannel 缺口与队列指标长期无样本而无人发现。补文件后重启 prometheus 即可。
- token 与 `platform-api` 的 `METRICS_ACCESS_TOKEN` 不一致时 prometheus 能起来但平台 job 无样本，触发 `PlatformMetricsSamplesMissing`（15m）。轮换 token 必须两侧同时改。
- 不发布宿主机端口：不要为抓取给 `platform-api`/`settlement-api` 增加 `ports:`，也不要改前端 nginx 增加 `/metrics` 路由；跨容器抓取只走 `application` 网络。

#### 4. XAPI-10 新键发布前核对（`SETTLEMENT_AUDIT_REQUIRED` / `PROJECT_AUDIT_REQUIRED`）

结算与项目新增了显式审计必开开关，判定顺序与平台 `platformAuditRequired` 同构：显式键 > `APP_ENV=production` > 生产环境编码（`PLATFORM_ENVIRONMENT_CODE=prod`）。生产编排里结算/项目的 `PLATFORM_ENVIRONMENT_CODE` 固定为 `prod`，因此**不写显式键也判定为“审计必开”**：凭据不全时结算 `/readyz` 返回 503、worker 对审计写入失败关闭。发布前逐项核对：

- [ ] 结算：`runtime/settlement.env` 的 `SETTLEMENT_AUDIT_CLIENT_ID`/`SETTLEMENT_AUDIT_CLIENT_SECRET` 已由受控接入下发（不是 `PENDING_ONBOARDING`）。确需临时豁免时显式写 `SETTLEMENT_AUDIT_REQUIRED=false` 并在验收记录写明理由，不要靠删键“恢复”。
- [ ] 项目：新键是 `PROJECT_AUDIT_REQUIRED`，兼容旧键 `PLATFORM_AUDIT_REQUIRED`（新部署只写新键）；审计机器客户端 `PLATFORM_AUDIT_CLIENT_ID`/`PLATFORM_AUDIT_CLIENT_SECRET` 已下发。显式值必须是 `true`/`false`，非法值按失败关闭处理（启动装配按 `true`）。
- [ ] 数析模板 `subsystem-templates/data-analysis.env.example` 的 `PLATFORM_AUDIT_REQUIRED=true` 与清单 `subsystems.d/data-analysis-prod.yaml` 的同一键保持一致，不要只改一侧。
- [ ] 发布后 5 分钟内 `SettlementAuditDestinationRequiredButUnconfigured` 未触发；Prometheus `/targets` 上 `platform-api`、`settlement-api`、`contract` 均为 UP；结算 `/readyz` 返回 200（degraded 时 `outbox_pending`/`unconfigured_destinations` 已有记录）。
- [ ] 结算通知目的地若本部署不需要，`SettlementNotificationDestinationUnconfigured`（6h）的豁免要在验收记录中写明，不要长期忽略告警。

### File Gateway 存储后端：仅本地 POSIX（S3/OSS/MinIO 未实现）

File Gateway **只实现了本地 POSIX 目录存储**，本文与其它部署文档中出现的“对象目录/存储根”
都是指宿主机目录挂载。必须按以下事实配置，不能按“支持对象存储”假设排期：

- `FILE_GATEWAY_STORAGE_BACKEND` 唯一可用值是 `local`。生产编排
  （`docker-compose.yml` / `compose.yaml` 的 `file-gateway` 服务）已把它固定为 `local`；
  设成 `s3` 等其它值不会降级，而是**直接启动失败**——
  `platform/cmd/file-gateway/main.go:38,58` 对未知后端返回
  `unsupported FILE_GATEWAY_STORAGE_BACKEND %q`。
- S3 / OSS / MinIO 后端**未实现**：没有实现代码、没有集成测试、没有发布路径。
- 历史遗留的 `FILE_GATEWAY_S3_*` 一组键（endpoint/bucket/region/AK/SK/session token/
  prefix/use_path_style/max_read_bytes）**已从两个模板移除**：
  `platform/.env.example` 与 `platform/deploy/production/.env.example` 现在只声明
  `FILE_GATEWAY_STORAGE_BACKEND=local`，不再声明任何 `S3_*` 键。
  所以“模板里找不到 S3 配置”是预期结果，不是漏配。
- 若将来要接对象存储，必须同时补实现、集成测试与迁移/回滚方案，属于另立项；
  在此之前不得在运维记录里写成“已支持对象存储”。

### File Gateway 8086 的受控直达入口（P0 验收用）

P0 验收需要直接在 File Gateway 上核对下载内容的 SHA-256。生产编排刻意**不**发布 8086，也不允许为了验收写成 `ports: 8086:8086`（那等于对 0.0.0.0 暴露内部 API 与 `/metrics`）。受控做法按优先级如下：

1. 首选（零暴露）：在容器内取流，SHA 计算留在宿主机。
   ```bash
   docker compose --env-file .env --env-file .release.env -f docker-compose.yml \
     exec -T file-gateway wget -qO- \
     --header="Authorization: Bearer $FILE_TOKEN" \
     http://127.0.0.1:8086/api/v1/files/<file_id>/content | sha256sum
   ```
   与上传时登记的 SHA-256 比对；再用无权/越权 token 重试，必须返回 401/403 且不得产生下载字节。
2. 备选（本机回环中继，验收后立即拆除）：在 `application` 网络上临时起一个**只绑定 `127.0.0.1`** 的中继容器（例如 `-p 127.0.0.1:18086:8086` 的 `socat` 中继），验收窗口结束执行 `docker rm -f`。禁止绑定 `0.0.0.0`、禁止改前端 nginx 代理 `/api/v1/files`、禁止把临时端口写进受控 compose 或覆盖文件。
3. 证据：受控入口的开启/关闭时间、使用身份与 SHA 比对结果写入验收记录；临时入口不得留在部署目录中。


注释系统只能保证其他服务可启动，不能保证依赖业务完整：合同缺失影响 CRM 合同交接；项目缺失影响合同交接和门户项目查询；CRM 缺失影响权威数据查询和门户补偿；结算缺失影响开票；门户缺失影响客户自助入口；看板缺失影响统计分析。缺失上游由动态 DNS 网关返回不可用状态，异步任务保持原有重试策略。

CRM/门户独立发布使用 `bin/deploy-customer-opportunity.sh customer-opportunity <digest>` 或 `customer-portal <digest>`；旧双 digest 入口仅用于两块都启用的兼容联合发布。

# 生产环境 CI/CD 部署

> 更新日期：2026-09-21。生产目录承载 platform、frontend、contract、CRM、客户 Portal、项目管理、数据看板和结算系统不可变镜像。

> 发布校验基线：平台 API 与 `subsystem-provisioner` 必须使用同一版本的 `PLATFORM_IMAGE`；平台应用授权仓储必须包含按 `client_id` 查询能力。若 CI 在 `OAuthClientManagementRepository` 报缺少 `GetOAuthClientByClientID`，说明镜像未包含最新平台提交，应先重新构建并推送不可变 digest，再发布。

## 1. 服务器要求

- Linux、Docker Engine、Docker Compose v2、Bash 4.3+、GNU coreutils、OpenSSL、`curl`、`gzip`、`tar`、`flock`；
- `iproute2`（提供 `ss`）与 `tzdata`；
- 低权限发布用户可访问 Docker，并拥有部署目录；
- Docker 统一前端网关终止 HTTP/HTTPS，只开放 SSH、80、443；MySQL、Temporal、Keycloak 管理端口和内部 API 不对公网发布；
- 部署目录默认 `/opt/unified-identity-platform`。

```bash
sudo install -d -o deploy -g deploy -m 750 /opt/unified-identity-platform
cd /opt/unified-identity-platform
cp .env.example .env
cp .release.env.example .release.env
install -d -m 700 runtime
chmod 600 .env .release.env
```

替换 `.env` 中所有基础设施占位值；`.release.env` 的镜像 digest 由 CI/CD 发布自动更新。`runtime/*.env` 不要求管理员手工创建：首次接入时，Agent 会从审核清单指定的 `*.env.example` 初始化缺失文件、自动收紧为 `0600`，写入 OIDC/授权目录/用途 Client，并为清单声明的业务密钥生成一次性 32 字节随机 base64 值。已有合法密钥、未知环境变量、注释及清单外文件都会保留，重试或更新不会轮换。部署人员也可以提前通过 Secret 管理系统写入合法密钥，Agent 会继续复用。首次镜像发布在接入凭据未补齐时只安全暂存 digest，不启动数据库迁移或 API。不要提交运行环境文件、私钥或备份。

Keycloak 的独立数据库、可选 bootstrap service account、备份恢复、HA、监控告警和灾备演练遵循 [Keycloak 生产运维 Runbook](../../docs/keycloak-production-operations.md)。公网 HTTP/HTTPS 模式统一由下述配置开关决定，不再手工维护宿主机 Nginx 或分别修改各子系统回调地址。

### 1.1 HTTP/HTTPS 快速切换

编排层缺省（未设置 `PUBLIC_HTTPS_ENABLED`）和显式 `false` 都使用 HTTP，且不会读取、校验或挂载任何证书。注意：本目录 `.env.example` 出于 fail-closed 考虑把模板值写成 `true`，直接复制模板后必须填写真实证书，否则预检失败；只有明确选择 HTTP 时才改为 `false`：

```dotenv
PUBLIC_HTTPS_ENABLED=false
PUBLIC_PLATFORM_HOST=platform.example.com
PUBLIC_SSO_HOST=sso.example.com
PUBLIC_HTTP_PORT=8081
PUBLIC_HTTPS_PORT=443
PUBLIC_SSO_HTTP_PORT=18090
PUBLIC_SSO_HTTPS_PORT=443
PUBLIC_HTTPS_DRAIN_GRACE=5m
PUBLIC_TLS_CERTIFICATE_PATH=
PUBLIC_TLS_PRIVATE_KEY_PATH=
SSO_TLS_CERTIFICATE_PATH=
SSO_TLS_PRIVATE_KEY_PATH=
```

`PUBLIC_SSO_HTTP_PORT` 和 `PUBLIC_SSO_HTTPS_PORT` 允许 Keycloak 与平台入口使用不同端口；留空时分别继承平台的 HTTP/HTTPS 端口。IP 直连部署应显式配置，例如平台使用 `8081`、Keycloak 使用 `18090`，避免 OIDC Issuer 被错误改写为平台端口。

启用 HTTPS 时，将 `PUBLIC_HTTPS_ENABLED` 改为 `true`，填写平台证书链和私钥路径。若 SSO 两项留空，SSO 复用平台证书，因此平台证书的 SAN 必须同时覆盖两个域名；也可以为 SSO 填写独立证书链和私钥。路径支持 Certbot 的符号链接，发布前会解析到可读常规文件，并校验证书链可解析、有效期、SAN 与私钥匹配。预检失败不会执行 Docker 变更。

```bash
cd /opt/unified-identity-platform
./bin/public-transport.sh check
./bin/apply-public-transport.sh
```

`apply-public-transport.sh` 是唯一的模式切换入口。它通过 migration 106 提供的数据库状态机持久化 `HTTP → ENABLING_HTTPS → HTTPS → DISABLING_HTTPS`，以数据库行锁防止并发执行，并可在脚本、容器或服务器重启后从原阶段继续。每次转换只记录模式、Origin、证书指纹/有效期、阶段、失败摘要和回滚结果，不记录证书正文、私钥、Token 或 Client Secret。

协调器先在同一数据库事务中为已有平台 OAuth Client 增加目标登录/退出回调，并记录受影响的 Application Environment、应用主页与 Keycloak Client；随后把 Keycloak Client 临时设置为新旧双回调。网关和业务运行配置健康后，协调器再事务性切换 Environment/主页地址、移除旧 OAuth 回调，并把 Keycloak 收敛到目标单回调。失败可回滚准备阶段，外部控制面成功前不会留下半套最终配置。

HTTPS 降级不会立即卸载证书。状态进入 `DISABLING_HTTPS` 后，统一 frontend 配置让 80/443 同时服务，业务服务改用 HTTP 与非 Secure Cookie，从 HTTPS 请求换发的 Cookie 可继续用于 HTTP；同时 `iam_session`、`oauth_token_family`、`oauth_refresh_token` 的绝对到期时间会被统一截断到“当前时间 + `AUTH_SESSION_TTL` + `PUBLIC_HTTPS_DRAIN_GRACE`”。截断前，协调器先把原值与截断值写入 `platform_public_transport_session_cap`，使失败的或被放弃的降级可以恢复（见 1.3）。截止时间前最终提交会被数据库协调器拒绝。截止后再次执行同一命令即可完成回调切换、停止 443 监听并保留受管证书供后续启用。证书路径在 `DISABLING_HTTPS` 结束前必须保持可读。

统一发布入口仅使用 `docker-compose.yml`，从传输配置派生 Origin、Issuer、回调和 Cookie 设置；HTTPS/排空由同一 frontend 服务与受管证书目录实现。

证书续期后只需执行以下命令重新校验证书并重建网关，不重启业务 API：

```bash
cd /opt/unified-identity-platform
./bin/reload-public-certificate.sh
```

本地开发同样支持该开关，配置位于 `platform/docker/.env.local`；默认仍保持 `http://localhost:8081`，只有显式启用 HTTPS 才加载 `platform/compose.https.yaml`。排空状态由协调器提供，`PUBLIC_TRANSPORT_STATE` 是内部恢复字段，不能作为日常开关手工长期保留。

### 1.2 OAuth Client Secret 吊销灰度（BIZ-RUNTIME-05）

受控更新与许可登记每次签发新 Secret 都会新增一行 ACTIVE 凭据，退役策略由 `AUTH_OAUTH_CLIENT_SECRET_PREVIOUS_RETIREMENT` 决定：`keep`（默认，不退役旧值）/`grace`（把旧值有效期收紧到窗口内）/`revoke`（立即吊销）。默认 `keep` 意味着旧 Secret 永久有效，泄露或容器遗留的 env 仍可换令牌，因此生产要按下面的灰度推进；三种策略的机制都已实现，本节是纯配置与运维流程。

运维入口与前置检查：

- 配置键写进部署目录的 `runtime/.env`（`platform/.env.example` 与 `deploy/production/.env.example` 都有带注释的模板），`platform-api` 与 `platform-worker` 都从该文件读取；改值后受控重启这两个服务。取值非法时进程拒绝启动（fail-closed），不是静默降级成 `keep`。
- `./bin/deploy.sh doctor` 新增只读检查 `[6/7] 旧 OAuth Client Secret 退役策略`：键缺失、取值非法、`grace` 缺窗口或窗口超出 `(0, 720h]` 都会给出 `[警告]`（不阻断部署）；`keep`/`revoke`/合法 `grace` 给出 `[OK]`，用于确认“以为开了灰度、实际还是 keep”的情况。
- `AUTH_OAUTH_CLIENT_SECRET_PREVIOUS_GRACE` 只对 `grace` 生效，取 Go duration 字面量（如 `24h`，上限 `720h` = 30 天）；`30d` 这类写法会被拒绝。`keep`/`revoke` 下该键被忽略。

**Phase 0 盘点（只读，不改库）**：先看 `/metrics`（`RequireMetricsAccess` 保护，10s 抓取缓存）：

| 指标 | 用途 | 告警判据 |
| --- | --- | --- |
| `platform_oauth_client_secret_active{status}` | 按状态统计的 Secret **总数**（全局分布） | 只作总览，不能用它反推单客户端累积 |
| `platform_oauth_client_secret_active_client{client_id}` | 单客户端 ACTIVE Secret 数量；按数量降序最多输出 200 个客户端序列（基数保护） | `> 1` 即该客户端有多个仍可换令牌的旧值；`grace`/轮换重叠窗口内属预期 |
| `platform_oauth_client_secret_active_clients` | 有 ACTIVE Secret 的去重客户端数 | 若大于 200，说明 `client_id` 序列被上限截断，需按客户端定向排查 |
| `platform_oauth_client_secret_oldest_active_seconds` | 最老 ACTIVE Secret 龄期（秒） | > 7776000（90 天） |
| `platform_oauth_client_secret_active_over_90d` | 龄期超过 90 天的 ACTIVE Secret 总数 | 衡量遗留面大小；告警以“最老龄期”为准 |
| `platform_oauth_client_secret_revoked_total` | 已吊销 Secret 总数 | 用于确认灰度收窗进度 |

采集失败时这一组指标整体缺席并计入 `platform_metric_collection_errors_total`，不会输出 0：看到指标缺失要先查采集链路，不能当成“龄期很新/没有累积”。没有任何 ACTIVE Secret 时 `oldest_active_seconds` 不输出（该维度无定义），而 `active_clients`/`active_over_90d` 输出 `0`（这是真实事实，样本缺席只代表采集失败或 `/metrics` 鉴权失效）。

对应告警规则（`monitoring/prometheus/platform-alerts.yml` 的 `platform-oauth-secrets` 组）：`PlatformClientSecretActivePerClientAccumulated`（单客户端 ACTIVE > 1，持续 1h；GRACE/重叠窗口内属预期）、`PlatformClientSecretActiveOldestOver90Days`（最老龄期 > 90 天，持续 6h）、`PlatformClientSecretGaugeSamplesMissing`（`up=1` 但 Secret gauge 无样本，抓取/采集失败兜底）。

**Phase 1 非生产开 GRACE**：在非生产 `runtime/.env` 设 `AUTH_OAUTH_CLIENT_SECRET_PREVIOUS_RETIREMENT=grace`、`AUTH_OAUTH_CLIENT_SECRET_PREVIOUS_GRACE=24h`（窗口上限 720h = 30 天；先跑一次 `./bin/deploy.sh doctor` 确认显示 `[OK] ... grace`），受控重启 `platform-api` 与 `platform-worker`，然后做一次受控更新并确认消费者在窗口内完成新 Secret 下发与重启。

**Phase 2 生产灰度**：先对非关键租户/客户端开 `grace`（24h→48h，窗口下限 = 最慢消费者重启耗时 + 一次受控更新重试）；稳定一周后对高敏客户端逐个收窗。**注意：HTTP 面没有请求级 `PreviousSecretPolicy` 字段**——全局策略之外的“单客户端立即退役”只能通过下面两个既有入口完成（都需要 `platform:oauth-client-credential:rotate` / `:disable` 权限，审计留痕）：

- `POST /api/v1/oauth-clients/{oauth_client_id}/credentials/rotate`，请求体 `{"overlap_seconds": 0, ...}`：同一事务内先吊销该客户端全部旧 ACTIVE Secret 再插入新值；`overlap_seconds` 非零则只把旧值有效期收紧到窗口内（≤ 30 天）。
- `POST /api/v1/oauth-clients/{oauth_client_id}/credentials/{credential_id}/disable`：按凭据 ID 精确吊销单个旧值（用来清掉 `keep` 期间累积的历史行）。

受控更新/许可登记（子系统接入流程）本身不带请求级策略，一律遵循全局配置；因此“高敏客户端立即退役”要么走上面的 rotate/disable，要么把全局策略切到 `revoke`（影响所有客户端，先演练）。

**Phase 3 密钥龄期制度**：最大 ACTIVE Secret 龄期 90 天；到期前用 `credentials/rotate`（重叠时长 ≤ 30 天）完成轮换，轮换后用 `platform_oauth_client_secret_active_client` 确认旧值已不在 ACTIVE 列表。

灰度回滚：把配置改回 `keep` 并重启即可停止继续退役；**已 REVOKE 的 Secret 不复活**（fail-closed），已收紧的 `valid_until` 也不会被回滚延长，需要恢复可用性只能重新签发新值。该灰度不涉及表结构迁移。

**发现与监控清单**：`keep` 是默认值且不阻断部署，所以“可发现”依赖三件事——`deploy.sh doctor` 的 `[6/7]` 策略检查（缺键/非法/窗口越界均警告）、上面的 `/metrics` 单客户端维度与龄期指标、以及 `platform-oauth-secrets` 组的三条告警。三者都缺失时不要假设“当前没有累积”。

### 1.3 HTTPS→HTTP 降级回滚（BIZ-RUNTIME-06）

降级在准备阶段会截断三张会话/令牌表的绝对到期时间。截断前协调器先把 `original_expires_at`/`capped_expires_at` 写入 `platform_public_transport_session_cap`（唯一键 `transition_id + session_table + session_id`），所以一次失败或运维放弃的降级不会固化成“全平台强制重新登录”。

```bash
cd /opt/unified-identity-platform
./bin/apply-public-transport.sh abort --yes
```

`abort` 等价于协调器 `fail --stage OPERATOR_ABORT`，`--yes` 是必须的显式确认；`fail` 命令保持兼容。协调器在一个事务内撤销准备阶段写入的目标回调、按 cap 表恢复被截断的到期时间、复位状态并清除 `drain_until`。要点：

- 只恢复“当前值仍等于截断值”的行：截断后被续期、重新登录或已自然过期的会话保持原状；这类行保留 cap 记录作为审计证据，已恢复的行删除记录。
- **恢复不复活窗口内自然过期的会话**：到期时间回到原值，但已经过去的时间不会变成未来。
- 控制面已提交（`CONTROL_COMMITTED`）后仍拒绝自动回滚，只允许前进恢复；此时 `abort`/`fail` 报错，请继续执行 `./bin/apply-public-transport.sh` 完成切换。
- 升级到本版本之前的降级切换没有 cap 记录，无法恢复（迁移不做历史回填）。运维细节见 [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md) 9.5。

本节是**一次性基础设施初始化**，由部署人员或 CI/CD 完成，不是每次接入子系统都要执行的管理员命令。Docker/Compose、镜像仓库访问、平台密钥、数据库、部署目录和隔离 Agent 准备完成后，日常平台管理员只使用基础平台“应用接入”页面。

应用接入不会在**预检阶段**要求所有平台级密钥都已经替换完成。预检只检查审核清单、模板、文件权限、镜像摘要和 Compose 配置；真正发布目标子系统时，Agent 只校验该目标声明的数据库凭据。当前六个生产目标分别需要 `CONTRACT_MYSQL_*`、`CUSTOMER_MYSQL_*`、`CUSTOMER_MYSQL_* + PORTAL_MYSQL_*`、`PROJECT_MYSQL_*` 、`SETTLEMENT_MYSQL_*` 或 `DASHBOARD_MYSQL_*`。数据库密码一旦用于持久化 MySQL，就不能由 Agent 随意生成或轮换，否则会导致已有数据库无法登录；因此它们仍需在第一次发布前由部署人员初始化。其他平台级密钥由平台自身启动和 CI/CD 初始化流程负责，不应阻塞无关子系统的预检。

测试服务器（一次性验证接入流程、没有历史数据）可在 `.env` 中设置 `SUBSYSTEM_PRODUCTION_ALLOW_PLACEHOLDER_DATABASE_CREDENTIALS=true`，Agent 将跳过数据库凭据校验，让 Compose 在首次创建空数据库时使用占位密码完成接入。该开关只应出现在测试服务器：已有持久化数据的数据库不会因为开关而自动改密，生产环境必须保持 `false`。

## 2. 镜像仓库

- `platform` 和 `frontend` workflow 使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`。
- `contract_management` workflow 与 `platform`/`frontend` 一致使用 ACR 变量（`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`）及 `ACR_USERNAME`/`ACR_PASSWORD` 凭据；服务器必须能够拉取对应 ACR 包。
- `project_management` workflow 与 `platform`/`frontend` 一致使用 ACR 变量：`ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据为 `ACR_USERNAME`、`ACR_PASSWORD`；服务器必须能够拉取对应 ACR 包。
- `data_analysis` workflow 同样使用 ACR 变量，单仓库多 tag 构建 `dashboard-api`、`aggregation-worker`、`alert-worker`、`production-migrate` 四个镜像，分别写入 `.release.env` 的 `DATA_ANALYSIS_*_IMAGE` 键。
- 远端发布统一使用 `image@sha256:digest`，不使用可变 tag 作为最终发布标识。

## 3. GitHub Environment

四个仓库的 deploy job 当前使用 `test` Environment。配置：

### Secrets

- `DEPLOY_HOST`
- `DEPLOY_USER`
- `DEPLOY_PORT`（可选，默认 22）
- `DEPLOY_SSH_KEY`
- `DEPLOY_KNOWN_HOSTS`
- ACR 仓库额外需要 `ACR_USERNAME`、`ACR_PASSWORD`

### Variables

- `DEPLOY_PATH`（可选，默认 `/opt/unified-identity-platform`）
- platform/frontend 仓库需要对应 ACR variables
- customer_and_opportunity 仓库可设置 `CUSTOMER_DEPLOY_SCRIPT`（默认 `/opt/unified-identity-platform/bin/deploy-customer-opportunity.sh`）

`DEPLOY_KNOWN_HOSTS` 必须在可信网络核对服务器指纹后生成。变量缺失时 deploy 任务会失败，不会跳过发布。

## 4. 首次上线

1. 发布 platform，使生产部署资产、平台镜像和迁移到位；
2. 初始化首个管理员；
3. 发布 frontend，并确认 `platform-api` 与隔离的 `subsystem-provisioner` 健康；
4. 发布所需子系统的不可变镜像。尚未接入时，发布脚本只安全暂存 digest，不会因 OIDC 占位值启动失败；
5. 登录基础平台，在“应用接入”中从服务器审核目标列表选择 `contract_management/prod`、`customer_and_opportunity/prod`、`customer_portal/prod`、`project_management/prod` 或 `data_analysis/prod`；客户 Portal 依赖 CRM，必须先完成 `customer_and_opportunity/prod`；数据看板的 `runtime/data-analysis.env` 需要预先配置与 Metabase 管理端一致的 `METABASE_EMBEDDING_SECRET`；
6. 平台自动创建应用环境、浏览器 Client、catalog-publisher Client、按用途拆分的服务 Client、精确回调和适用的初始管理员授权；Agent 自动初始化对应 `runtime/*.env`、生成清单声明的长期业务密钥并写入一次性凭据，再按目标执行固定备份、迁移和 API 重建。

生产接入不再要求管理员在命令行复制 OAuth Client Secret。Secret 只在平台后端内存、受限 Unix Socket 和权限为 `0600` 的服务器 `runtime/*.env` 之间流转，不返回浏览器，也不进入命令行参数或日志。生产 Agent 只允许 `subsystems.d/*.yaml` 中随发布包审核的应用/环境和固定 Compose 服务，不接受浏览器指定的文件、命令、镜像或服务名。

首次接入前，`runtime/contract.env` 中的以下字段可以保留占位值，由平台接入流程替换：

```text
OIDC_CLIENT_ID
OIDC_CLIENT_SECRET
OIDC_TENANT_ID
PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID
PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET
PLATFORM_AUDIT_CLIENT_ID
PLATFORM_AUDIT_CLIENT_SECRET
```

`SUBSYSTEM_PRODUCTION_HOST_DEPLOY_ROOT` 必须填写当前生产部署目录的规范绝对路径，默认 `/opt/unified-identity-platform`。`SUBSYSTEM_PRODUCTION_PROFILES_DIR` 默认 `/opt/unified-identity-platform/subsystems.d`，必须位于部署根内、不能是符号链接，目录和清单不能组/全局可写。`SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID` 默认对应迁移内置租户，标准单租户部署不需要额外配置。平台镜像更新后会同时重建 `platform-api` 和 `subsystem-provisioner`，二者分别只读同一清单并通过共享 Unix Socket 通信；只有 docker-socket-proxy 挂载 Docker Socket；Agent 经受限代理 API 操作 Docker，不直挂宿主 Socket。

### 4.1 新增生产子系统目标（部署人员）

新增子系统时不再向 `.env` 增加一组 `SUBSYSTEM_PRODUCTION_APPLICATION_*` 白名单。部署人员应在代码评审中新增 `subsystems.d/<application>-<environment>.yaml`，并同步准备 Compose 服务、不可变镜像键和 `subsystem-templates/*.env.example`。该模板目录整体只读挂载给 Agent，后续新增模板不需要再修改 Compose volume。清单只允许声明：

- 应用编码、环境、固定 PathPrefix/UpstreamURL 和客户端类型；
- 部署根 `runtime/` 下的环境文件、受控初始化模板、可首次生成的 base64 密钥，以及平台输入到明确环境变量的绑定；
- 固定依赖、数据库备份目标、迁移服务、运行服务和下线服务（实际服务必须存在于统一编排）；
- `.release.env` 中必须为 `image@sha256:digest` 的镜像键。

清单不能声明 shell 命令、脚本、任意宿主机绝对路径，也不能选择 `platform-api`、`platform-mysql`、`frontend`、`subsystem-provisioner`、`PLATFORM_IMAGE` 或 `FRONTEND_IMAGE`。未知 YAML 字段、重复应用/环境、未知凭据来源、路径逃逸、符号链接和不安全权限都会使平台或 Agent 启动失败。当前随包审核目标见 [`subsystems.d/`](./subsystems.d/)；发布清单后管理页面会自动出现新选项，无需逐台服务器手工维护应用白名单。

最小示例：

```yaml
version: 1
default: false
application:
  code: billing
  name: 计费系统
  description: 计费与对账
  environment: prod
  path_prefix: /billing
  upstream_url: http://billing-api:8080
  client_type: confidential
runtime:
  # 仅声明该子系统实际使用且不可安全自动轮换的数据库凭据。
  # 这些键在真正 Provision 前校验，不会因为预检阶段仍有平台级占位值而阻止登记接入。
  required_infrastructure_keys: [BILLING_MYSQL_PASSWORD, BILLING_MYSQL_ROOT_PASSWORD]
  files:
    - path: runtime/billing.env
      template_path: subsystem-templates/billing.env.example
      compose_environment_key: BILLING_RUNTIME_ENV_FILE
      required_existing_keys: []
      generated_keys: [BILLING_ENCRYPTION_KEY_BASE64]
      values:
        OIDC_SCOPES: openid profile
      bindings:
        OIDC_ISSUER: issuer
        OIDC_CLIENT_ID: client_id
        OIDC_CLIENT_SECRET: client_secret
        OIDC_REDIRECT_URI: redirect_uri
        OIDC_TENANT_ID: tenant_id
        OIDC_SESSION_COOKIE_SECURE: cookie_secure
        PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID: application_id
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID: catalog_publisher_client_id
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET: catalog_publisher_client_secret
        PLATFORM_AUDIT_CLIENT_ID: service.audit_ingest.client_id
        PLATFORM_AUDIT_CLIENT_SECRET: service.audit_ingest.client_secret
compose:
  dependency_services: [billing-mysql]
  database: {service: billing-mysql, name: billing}
  migrate_service: billing-migrate
  runtime_services: [billing-api]
  teardown_services: [billing-api]
  release_image_keys: [BILLING_IMAGE]
```

`template_path` 是部署根内随版本审核的 `*.env.example` 相对路径；缺失的 runtime 文件会从这里原子初始化。`generated_keys` 只接受明确声明的 `*_KEY_BASE64` / `*_PEPPER_BASE64`，仅在键缺失或仍为 `REPLACE_WITH_*` / `PENDING_*` 时生成，已有合法值不覆盖。`bindings` 的值不是模板表达式，而是 Agent 内置的有限数据源。通用来源包括 `issuer`、`client_id`、`client_secret`、`redirect_uri`、`public_url`、`tenant_id`、`application_id`、`application_code`、`environment`、`path_prefix`、`cookie_secure` 和 catalog-publisher 凭据；用途 Client 使用 `service.<purpose>.client_id` / `service.<purpose>.client_secret`。如果子系统需要新的机器用途，应先在平台控制面增加最小 scope 的用途 Client，再在清单引用，不能复用浏览器 Client。

Agent 采用“只管理声明键”的兼容策略：子系统以后新增环境变量、注释、证书或其他文件不会因为未列入清单而被删除或拒绝；需要平台生成/注入的新 runtime 文件时，只增加模板和 YAML 文件项，无需修改 Agent Go 代码。仍需严格声明的是宿主机写入目标、平台凭据映射和 Compose 服务，浏览器不能动态指定这些高权限操作。

完成上述一次性服务器初始化后，Application、Environment、登录目标、OAuth Client、运行时凭据、首次管理员授权、失败重试和安全下线都从基础平台页面操作。接入时未另选初始管理员则使用当前操作者；平台会保存这一选择，首次部署失败后的页面重试仍使用原选择，不会改授给点击重试的人。初始授权完成后，普通更新或重试不会恢复后来主动移除的角色。

日常管理员不需要登录服务器、执行子系统接入脚本或在命令行复制 OAuth 配置；服务器命令仅保留给 CI/CD、首个管理员初始化和基础设施故障恢复。部署失败时先读取环境卡片的脱敏错误与“下一步操作”，修复后点击“重试”，不要重复新增接入。

### 4.2 客户与商机接入返回 503 时

`PLATFORM_DEPENDENCY_UNAVAILABLE` 只是平台对外的安全错误码，不代表一定是 CRM API 本身故障。先在服务器检查 Agent、目标依赖和目标 API：

```bash
cd /opt/unified-identity-platform
docker compose --env-file .env --env-file .release.env \
  -f docker-compose.yml ps subsystem-provisioner customer-mysql customer-api customer-migrate
docker compose --env-file .env --env-file .release.env \
  -f docker-compose.yml logs --tail 200 subsystem-provisioner customer-mysql customer-api customer-migrate
```

重点核对：

1. `platform-api` 与 `subsystem-provisioner` 必须使用同一个最新 `PLATFORM_IMAGE`，并且生产清单目录包含 `subsystems.d/customer_and_opportunity-prod.yaml`；只更新平台 API、不重建 Agent 会导致目标被旧 Agent 拒绝。
2. `.release.env` 必须包含 `CUSTOMER_CRM_IMAGE=image@sha256:<64位摘要>`；不能使用普通 tag 或占位值。
3. 不再需要手工创建 `runtime/customer.env` 或填写三个长期业务密钥。新版 Agent 会从 `subsystem-templates/customer.env.example` 初始化、自动收紧到 `0600`，并首次生成 `SENSITIVE_ENCRYPTION_KEY_BASE64`、`SENSITIVE_HMAC_KEY_BASE64`、`PORTAL_INVITE_PEPPER_BASE64`；若仍提示 runtime/模板错误，通常是 Agent 仍运行旧镜像、模板目录未随生产资产发布或 `runtime/` 不可写。
4. 若日志显示数据库、迁移或 API 健康检查失败，先处理对应容器；若显示 `deployment helper is unavailable`，先恢复 Agent；若显示 `target is not allowed`，同步完整生产部署资产并同时重建 `platform-api`、`subsystem-provisioner`。

请求响应中的追踪号会同步写入 Agent 日志的 `request_id` 字段。预检失败时控制面尚未创建应用环境，应修复后重新提交接入；只有已经生成环境并进入 `PROVISION_FAILED` 时才点击页面上的“重试”，不要重复 onboard。

管理员初始化：

```bash
cd /opt/unified-identity-platform
read -rsp "管理员密码: " ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" | docker compose \
  --env-file .env --env-file .release.env \
  run -T --rm platform-migrate ./bootstrap-admin \
  --display-name "平台管理员" --account-name admin --password-stdin
unset ADMIN_PASSWORD
```

生产合同回调应使用：

```text
https://<正式域名>/contract_management/auth/callback
```

不要把本地 `dev` Client、localhost 回调或局域网 HTTP 配置复制到生产，也不要手工修改平台数据库伪造 `READY` 状态。

## 5. 发布行为

CI 远端调用：

```bash
./bin/deploy-service.sh platform <platform-image@sha256:digest> <file-gateway-image@sha256:digest>
./bin/deploy-service.sh frontend <image@sha256:digest>
./bin/deploy-service.sh contract <image@sha256:digest>
./bin/deploy-service.sh project <image@sha256:digest>
./bin/deploy-customer-opportunity.sh <crm-image@sha256:digest> <portal-image@sha256:digest>
```

前端发布使用统一 `docker-compose.yml`，仅重建 frontend 服务。
前端工作流同步到服务器。

脚本使用 `flock` 串行发布，校验 Compose，后端发布前备份数据库，执行迁移，更新单个服务并检查健康状态。应用失败会恢复上一镜像；已成功执行的数据库迁移不会自动反向迁移。首次发布 contract 或 project 且接入凭据仍为占位值时只保存不可变镜像指针，实际迁移和启动由基础平台页面接入触发。

CI 发布不删除或重建 Application、Environment、LoginTarget、OAuth Client；资产安装保护运行 Secret，但发布会更新 `.release.env`，并按发布链路受控更新 runtime 的目录哈希／管理键，不能理解为这些文件永不改写；它会更新随代码审核的 `subsystems.d` 清单。首次接入及受控更新／重试会更新目标清单明确列出的运行时字段，其他长期业务密钥保持不变。

项目管理系统发布显式运行 project-migrate，不影响既有 platform/frontend/contract 发布。首次发布 project 且 `runtime/project.env` 仍含 `PENDING_*`/`REPLACE_WITH_*` 时只安全暂存 `PROJECT_IMAGE` digest，接入完成后再由 Agent 迁移并启动 `project-api`。

客户与商机旧双 digest 入口用于联合发布，独立发布采用本文顶部单模块命令；不影响既有 platform/frontend/contract 发布。脚本先校验两个 ACR 不可变 digest，然后更新并备份 `.release.env`，并把 CRM 镜像内嵌的授权目录哈希原子写入 `runtime/customer.env`。如果 `.env`、`runtime/customer.env` 或 `runtime/portal.env` 仍含 `REPLACE_WITH_*`/`PENDING_*`，脚本只暂存镜像，不启动服务。配置完整后依次启动双库、生成各库逻辑备份（不是跨库同一时点一致性快照）、执行两个 schema 的语句级迁移，再切换 CRM、Portal API、CRM Workers 和 Portal 邀请补偿 Worker，并检查健康/运行状态；失败时这些服务统一回滚。

测试服务器继续使用非回环 HTTP 时，在 `.env` 设置 `SUBSYSTEM_ALLOW_INSECURE_HTTP_SESSION=true`。
完成 HTTPS、Redirect URI 和 Secure Cookie 迁移后改为 `false`；该部署级开关会统一覆盖 CRM 与
Portal 的 HTTP 会话例外，清单不再永久写死允许值。

语句级迁移表为 `app_schema_migration_statements`。每条 SQL 在执行前记录 `RUNNING`，成功后才改为 `APPLIED`；重启发现遗留 `RUNNING` 会拒绝继续。必须人工核验对应表、列、索引、约束和必要回填后，按变更评审结果处理该检查点，禁止把 duplicate table/column/index 笼统视为成功。迁移器只自动接管空 schema；已有业务表但没有生产元数据表时拒绝发布，必须先建立经评审的生产基线。

### 安装或升级客户商机生产资产

通过已校验部署资产包和 install-assets.sh 事务安装完整资产，不能以直接覆盖单个脚本／清单代替协调升级。完成门禁处理后执行：

```bash
cd /opt/unified-identity-platform
install -d -m 700 runtime backups backups/releases
chmod 600 .env .release.env
chmod 750 bin/deploy-customer-opportunity.sh
```

随后从基础平台“应用接入”页面执行首次接入；Agent 会初始化缺失的 runtime 文件。已有运行配置禁止用模板覆盖，Agent 只更新清单管理键并保留新增字段。更新 Nginx 后先执行 `nginx -t`，再 reload。GitHub `test` Environment 的 `CUSTOMER_DEPLOY_SCRIPT` 必须与实际安装绝对路径一致。

## 6. 恢复和备份

完整生产灾备、统一部署锁、File Gateway 一致性快照、恢复 UID/GID、加密归档、只校验及空目录重建演练的操作步骤见 [BACKUP_RECOVERY.md](BACKUP_RECOVERY.md)。灾备入口为 `bin/backup-all.sh`；它不仅备份数据库，还保存受限或加密的运行配置、平台签名密钥、受管 TLS 和不可变镜像版本清单。

必须备份：

- platform MySQL；
- contract MySQL；
- 平台上传文件；
- JWT 密钥；
- `.env`、`.release.env` 和 `runtime/contract.env` 的安全副本；
- `runtime/customer.env` 与 `runtime/portal.env` 的安全副本；
- 生产 Nginx 配置。

恢复演练要验证数据库、文件、Issuer、Client 凭据和上一镜像能共同恢复；只回退镜像不能逆转不兼容迁移。通用 MySQL 恢复会在统一部署锁内检查全部实际运行写入者；恢复 `contract-mysql` 还需要独立 Temporal 维护确认，文件网关数据库禁止脱离对象文件单独恢复。

### 6.1 Keycloak 数据库逻辑备份与恢复

生产包提供两项不改变网络入口、不会强制 HTTPS 的运维资产：

- `bin/backup-keycloak-mysql.sh`：在线一致性逻辑备份，校验 gzip、非空文件和 SHA-256；默认不删除历史备份。
- `bin/restore-keycloak-mysql.sh`：先做备份完整性、校验和、Compose、数据库健康与凭据占位符检查；真正导入需要显式破坏性确认，且拒绝在 Keycloak 仍运行时执行。

安装时仅需收紧目录和脚本权限；不要把备份、`.env` 或 runtime Secret 提交到 Git：

```bash
cd /opt/unified-identity-platform
install -d -m 700 backups/keycloak monitoring/textfile
chmod 750 bin/backup-keycloak-mysql.sh bin/restore-keycloak-mysql.sh
chmod 600 .env .release.env
```

备份命令可以在线执行。它在 `keycloak-db` 容器内读取数据库 root 密码，因此密码不会出现在宿主机命令行、cron 参数或日志中：

```bash
cd /opt/unified-identity-platform
./bin/backup-keycloak-mysql.sh
```

恢复必须先在**隔离演练环境**验证。先只校验备份，不会修改数据：

```bash
cd /opt/unified-identity-platform
./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-YYYYMMDDTHHMMSSZ.sql.gz \
  --verify-only
```

获批的实际恢复是破坏性操作：先冻结认证变更、停止全部 Keycloak 节点，并确保已准备同一时间点的受控 Secret、入口配置和镜像 digest。脚本不会自行停止或启动容器，避免在未知拓扑中误操作；只有输入固定确认文本才会清空并导入独立 Keycloak 数据库：

```bash
cd /opt/unified-identity-platform
# 仅在已完成隔离演练并获得变更审批后执行：
./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-YYYYMMDDTHHMMSSZ.sql.gz \
  --confirm RESTORE_KEYCLOAK_DATABASE
```

导入成功不代表恢复完成。恢复后使用备份匹配的受控配置启动经批准的 Keycloak 镜像，再验证数据库健康、`/health/ready`、Issuer discovery、Realm、Client、JWKS 和一次测试账号登录；同时记录实际 RTO/RPO。脚本只接受 `backups/keycloak` 目录中的 `keycloak-*.sql.gz`，默认拒绝符号链接与未停止 Keycloak 时的导入。

## 商业授权组件更新

先运行平台真实迁移至 118，再发布支持受控生命周期的 API 和 Agent。镜像或覆盖摘要变化由审核后的固定 `runtime-license-<app>-<env>.json` 驱动；普通用户及 runtime Token 不能提交组件删除清单。平台接入重试会核对完整批准集合、生成新机器身份、版本锁定替换并保留审计，旧机器身份立即失去协调接口权限。替换不恢复存量迁移资格，全部有效成员需重新确认。

合同使用 `contract-api` 与 `contract-worker` 两服务，同镜像但分别配置 `runtime/license-contract-api.env`、`runtime/license-contract-worker.env` 和独立持久卷。新 profile 两者均为必需运行单元；不能只批准 API 或让 Worker 复用 API 的 Secret。默认 split 禁止内嵌 Worker，迁移和 Temporal 健康检查先于两组件启动，Worker `/readyz` 端口不对公网发布。退役不删除数据库、授权状态卷或业务文件。

审核清单可提供 `release_generation`（回滚或恢复旧逻辑服务必须使用新的批准代次）与 `retire_service_ids`（明确允许的旧成员）。清单未声明的成员不能退役；启用中的必需/条件组件不能退役。此流程的协议和隔离数据库测试不代替全栈安装验收。

API 与 Agent 应同步升级：每组件交付需携带完整审核摘要，Agent 在写凭据和停止进程前严格复核组件、必需成员、退役名单及代次，缺失摘要或审核文件漂移都会拒绝。状态文件名绑定实例、环境、机器身份和镜像/覆盖摘要，新身份不能继承旧迁移缓存；同身份 Secret 轮转保留持久状态。退役只停止经完整容器 ID 和项目/服务标签复核的本应用进程，不删除容器、状态卷或业务数据；跨应用及基础设施名单被拒绝。

## 2026-10-13 当前操作判据

本文由生产运维执行发布／恢复，应用管理员执行所属租户接入，业务人员执行真实业务验收。
本次只核验源码，没有新部署或动态验收；文内测试／发布命令均是待执行步骤。

资产安装持 runtime/.deploy.lock 并备份静态资产，这不是全系统数据库灾备；中文升级入口和底层发布另执行 backup-all。
已有 Compose 通常写 .dist 供审阅，但安装器会受控迁移旧 Socket Proxy 镜像、frontend 地址表达式和批准目录挂载，不能称为活动 Compose 绝不修改。
资产事务残留先 install-assets.sh --recover；控制面普通重载与绑定新版平台镜像的协调升级按不同门禁处理，不手删标记。

平台发布使用平台／网关两摘要，数据分析使用四个 target 摘要；只有声明齐全才能采用。
prepare 创建不启动的候选；continue 只在已受控接入且 READY_TO_CONTINUE 时清理候选，不执行部署或新迁移。
install 在普通路径暂存业务候选，带匹配许可及审核安装计划时有专用自动采用路径。
业务升级先暂存新摘要，再由页面更新；平台／前端 upgrade 则实际执行发布，不能统一描述成只暂存。

页面采用／更新需要 application:update、application-environment:update、application-login-target:update、oauth-client:disable；
重试另需 role-binding:update，完整权限均以 platform: 为前缀。状态／候选读取需要 application:read。
后台按租户解析应用环境、校验目标／认证／清单，事务锁定部署代次，受控交付凭据，Agent 写 runtime、备份、迁移和启动所有声明服务，
最后保存已应用清单／初始访问／终态。请求受理不等于 READY，也不等于真实登录可用。
失败保留数据和错误证据；生产 Agent 没有可以一概保证的全流程旧镜像／SQL 自动回滚。

start/resume 会走发布、备份和迁移，不是仅 docker start。基础 verify 的网关匿名接口接受 401 或 405，不代表授权业务请求通过。
时区例外使用 runtime/compose.timezone.yaml，包装脚本和新版 Agent 会加载；HTTPS／排空仍使用受管统一前端，不另开旧覆盖流程。

专用 Keycloak 恢复的 verify-only 仍需 Docker／数据库，实际恢复缺少通用恢复的全容器写入者及统一锁门禁；
优先按 [备份恢复](BACKUP_RECOVERY.md) 采用完整批次。多节点须人工全部停写，单库导入失败没有 SQL 自动回滚。

## 2026-11-05 升级期新增语义（数据库重建与验收等待）

平台阶段在数据库容器被重建后会强制重建其强依赖（`keycloak-db -> keycloak`、`contract-mysql -> contract-api / contract-worker`），
并把判定持久化到 `runtime/database-refresh-state`；刷新失败写 advisory 标记 `runtime/database-refresh-failed.json`，
由 `deploy.sh status` / `verify` 展示、不阻断发布。健康收敛改为「每服务窗口 `UIP_HEALTH_WAIT_SECONDS` + 总预算 `UIP_HEALTH_TOTAL_SECONDS`」，
`RestartCount` 只判窗口内新增。Keycloak 探针改用管理端口 `/health/ready`，发布证明另做 OIDC 发现文档两级判定（master 硬性、租户 realm 404 仅告警）。
Temporal 只在启用合同 / 项目 / CRM 任一消费子系统时启动，`verify` 门禁同步条件化。完整说明见 `OFFLINE_RUNBOOK.md` 9.6。
