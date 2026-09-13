# 子系统接入可靠化改造方案（完整版）

> 状态：**方案已定稿，尚未实施**（本文只描述方案，不含任何代码改动）
> 更新日期：2026-09-12
> 适用对象：本地开发与联调人员、平台部署脚本维护者、架构评审、**后期服务器部署**
> 关联文档：[子系统开发与统一身份接入手册](./subsystem-onboarding.md)、[本地 Docker 与脚本使用说明](./local-docker-operations.md)、[生产环境 CI/CD 部署](../deploy/production/README.md)

---

## 1. 背景

本地重新部署（`down --volumes` 后 `up`，或平台库被以其它方式重建）时，子系统接入基础平台这一步经常失败，且失败信息（HTTP 401、容器 unhealthy）指向不了真正原因。

本方案把该现象定位为四条结构性原因，给出可实施的设计，并**按"本地 + 服务器"双场景**统一：同一套清单模型、同一个工具，只有**凭据来源**按环境切换。

## 2. 问题诊断（均有本地实测证据）

### 2.1 反向漂移没有任何防护（根因）

接入状态实际分散在两个地方：

| 面 | 载体 | 内容 |
| --- | --- | --- |
| 控制面 | 平台库（MySQL 命名卷） | `platform_application`、`platform_application_environment`、`platform_oauth_client`、`subsystem_deployment_state` 等 |
| 运行面 | 各子系统 env 文件（宿主机） | `OIDC_*`、`PLATFORM_AUTHORIZATION_CATALOG_*`、各机器凭据 |

`docker-local.sh` 的 `down --volumes` 分支执行 `compose down --volumes --remove-orphans`，**删除全部命名卷（含平台库）**，而宿主机 env 文件原样保留。

脚本目前只防住了反方向：`ensure_platform_env_file` 在"数据卷存在但 env 文件缺失"时直接 `fail` 并提示不要删卷。**"env 文件存在、平台库被删"这一方向没有任何防护**——而这恰恰是"我想重新部署"时最常见的路径。

**实测证据（2026-09-12 本地）**

- `platform_application` 仅剩 `platform` 与 `contract_management`；`platform_oauth_client` 为 **0 行**。
- 而四个子系统的 env 文件仍持有旧凭据。
- 用 `data_analysis` 旧凭据请求 `POST /oauth2/token`（`grant_type=client_credentials&scope=authorization.catalog.sync`）返回 **401 `invalid_client`**，与容器内报错逐字一致。
- 容器表现：CRM / Portal 在脚本的 `authz-catalog publish` 处失败；`dashboard-api` 在**容器内启动期自同步**处失败并重启循环（其 `cmd/authz-catalog` 是占位实现，脚本层无法代它发布）。

### 2.2 接入无法无人值守

`subsystem.sh` 的 `ensure_authenticated` 被 `cmd_onboard` / `cmd_update` / `cmd_status` / `cmd_offboard` 共用（已逐一核对）：没有 `--cookie-file` 就必须登录。

本地首次初始化接收的管理员口令在 `docker-local.sh` 中用完即 `unset`；生产 `deploy/production/README.md` 同样是交互输入后即 `unset`。`docker/.env.local` 中的 `IAM_BOOTSTRAP_TOKEN` 只用于创建首个管理员（`X-Bootstrap-Token` 头），**不能替代管理员会话**。

### 2.3 参数靠手敲

只有两个快捷预设（`contract-management-local`、`customer-portal-local`）。其余子系统要手填 6–8 个参数；其中 `--upstream-url` 必须是**容器内**地址，最容易填错且错了只报 401/404。

### 2.4 本地缺少生产已有的声明式模型

生产已有 `deploy/production/subsystems.d/*.yaml`（5 个子系统，含 `application` / `runtime.files` / `runtime.generated_keys` / `runtime.values`）+ `subsystem-templates/*.env.example`；本地是一堆手写 `ensure_*_env_file` 函数加人工 onboard。**两套模型并存**。

### 2.5 失败发现得太晚

没有任何入口能回答"哪些子系统已接入、凭据是否还有效、下一步该执行什么"，只能等容器 unhealthy。

## 3. 目标与非目标

**目标**

- **G1**：`down --volumes` 后的重新部署行为可预期，不再出现"配置齐全、凭据全废"的静默漂移。
- **G2**：接入状态可见——一条命令给出每个子系统的控制面状态、凭据有效性、下一步动作。
- **G3**：可无人值守——重新部署不需要人工敲 6–8 个参数。
- **G4**：同一套模型与工具可复用到服务器部署；生产默认只出计划、显式 `--apply` 才写；全程可审计。

**非目标**

- 不改变生产接入流程与控制面边界：页面与 `subsystem-provisioner` 仍是唯一入口。
- 不让任何 Secret 进入 Git、命令行参数或普通日志。
- 本期不修改各子系统业务代码。

## 4. 决策记录

| # | 决策 | 理由 |
| --- | --- | --- |
| 1 | 管理员凭据按环境区分，**服务器不落盘人类口令** | 本地落盘换自动化；服务器改用专用运维服务账号 + Secret 注入，避免长期驻留人类口令 |
| 2 | 一套 schema + 一个 reconciler，**清单目录按环境分离** | 要统一的是模型与工具，不是路径；不动生产目录可零风险，避免触碰现有发布链路 |
| 3 | 保留 `subsystem.sh onboard`，但降级为"首次接入 / 排障"路径 | 审计与排障仍需手工路径；常规重建改走 `reconcile` |
| 4 | 方案 D 清空**全部**接入产物，且不夹带凭据轮换 | 半清会留下"看似已接入"的中间态，正是漂移来源；"消除不一致"与"轮换凭据"必须分成两件事 |
| 5 | 方案 E 本期不做，设定触发条件 | 需改 4 个子系统仓库；收益已被平台侧"跳过"基本覆盖；触发条件：生产出现"子系统目录同步失败导致整套发布回滚" |

### 决策 1 的权限设计（服务账号最小权限，权限码取自路由实测）

| 用途 | 权限码 |
| --- | --- |
| 读控制面 | `platform:application:read`、`platform:application-environment:read`、`platform:application-login-target:read`、`platform:oauth-client:read` |
| 接入 / 更新 | `platform:application:create`、`platform:application-environment:create`、`platform:application-login-target:create`、`platform:oauth-client:create`、`platform:role-binding:update`、`platform:application:update`、`platform:application-environment:update`、`platform:application-login-target:update`、`platform:oauth-client:disable` |
| 仅启用方案 C 时需要 | `platform:oauth-client-credential:rotate` |
| **明确不授予** | `platform:application:delete`、`platform:application-environment:delete`（避免自动化误删） |

## 5. 技术底稿（实施者需要的既有事实）

### 5.1 控制面 API（均已存在，无需新增端点）

| 端点 | 权限 | 用途 |
| --- | --- | --- |
| `GET /api/v1/applications` | `platform:application:read` | 列出已登记应用 |
| `GET /api/v1/applications/:id/environments` | `platform:application-environment:read` | 列出应用环境 |
| `GET /api/v1/subsystem-status` | `platform:application:read` | 单子系统部署状态（`subsystem.sh status` 已在使用） |
| `POST /api/v1/subsystem-onboarding` | create 系列 5 项 | 首次接入 |
| `POST /api/v1/subsystem-update` | update 系列 4 项 | 更新运行时配置 |
| `POST /api/v1/subsystem-retry` | — | 失败重试 |
| `POST /api/v1/subsystem-deployment-discard` | update 系列 4 项 | 丢弃停在 `PROVISION_FAILED` 的记录 |
| `POST /api/v1/oauth-clients/:id/credentials/rotate` | `platform:oauth-client-credential:rotate` | 轮换并重新下发 secret（方案 C 用） |

### 5.2 认证方式（`subsystem.sh`）

`--account` + `--password-stdin`（推荐）或交互输入；`--cookie-file FILE` 复用已登录会话；`--dry-run` 仅校验不登录。

### 5.3 现有可复用能力

- `platform_catalog_token_status <client_id> <secret>`（本会话新增）：经 frontend 容器内 wget 换一次目录同步令牌，返回 HTTP 状态码。**只换令牌、不写数据**，是现成的凭据有效性探针。
- `docker-local.sh` 的 `env_value` / `replace_line_in_file` / `compose_run` / `ensure_*_env_file`。
- `compose.local.yaml` 中 **6 个子系统 API 服务**（`contract-api`、`customer-api`、`portal-api`、`project-api`、`dashboard-api`、`settlement-api`）已带 `com.basic-platform.application_code`、`path_prefix`、`health_endpoint` 等标签，正好覆盖 reconciler 需要比对的全部服务，可直接用于清单与编排的一致性校验。

### 5.4 各子系统接入产物键名（清理与探测的精确范围）

| 子系统 | env 文件 | 接入产物键（前缀/全名） |
| --- | --- | --- |
| contract_management | `contract_management/.env.local` | `OIDC_CLIENT_ID`、`OIDC_CLIENT_SECRET`、`OIDC_TENANT_ID`、`OIDC_REDIRECT_URI`、`OIDC_CLIENT_ID_ROLLBACK`、`OIDC_CLIENT_SECRET_ROLLBACK`、`OIDC_ISSUER_ROLLBACK`、`PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID`、`PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID`、`PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET` |
| customer_and_opportunity | `docker/.env.customer.local` | 同上 + `OIDC_ROLE_CONFIG_HASH`、`MACHINE_TOKEN_*` |
| customer_portal | `docker/.env.portal.local` | `PORTAL_OIDC_CLIENT_ID/SECRET/TENANT_ID/REDIRECT_URI`、`PORTAL_ROLE_CONFIG_HASH`、`PORTAL_AUTHORIZATION_CATALOG_*`、`PORTAL_MACHINE_TOKEN_*` |
| project_management | `project_management/.env.local` | `OIDC_CLIENT_ID/SECRET/TENANT_ID/REDIRECT_URI`（含 `_ROLLBACK` 变体）、`PLATFORM_AUTHORIZATION_CATALOG_*` |
| data_analysis | `data_analysis/.env.local` | `OIDC_CLIENT_ID/SECRET/TENANT_ID/REDIRECT_URI`、`PLATFORM_AUTHORIZATION_CATALOG_*`、`CONTRACT_MACHINE_CLIENT_ID/SECRET`、`PROJECT_MACHINE_CLIENT_ID/SECRET`、遗留 `MACHINE_CLIENT_ID/SECRET` |

> **设计约束**：上表不写死在代码里，而是**声明在各子系统的清单文件**中（见 7.1 `onboarding_artifacts`），使清理与探测都是数据驱动的，新增子系统不需要改脚本。

## 6. 总体设计

```
                    deploy/{local,production}/subsystems.d/*.yaml   ← 唯一真相（声明）
                                    │
                    ┌───────────────┴───────────────┐
                    │   subsystem.sh reconcile      │  ← 唯一工具
                    └───────────────┬───────────────┘
                                    │
        ┌───────────────┬───────────┴───────────┬───────────────┐
        │               │                       │               │
   doctor 探测      动作执行              凭据提供层        审计摘要
   ├ 控制面状态     ├ onboard            ├ 本地 secret     └ stdout(表) +
   ├ 凭据有效性     ├ reset→onboard      ├ --password-stdin    JSONL(留痕)
   ├ 网关一致性     ├ update             └ --cookie-file
   └ 清单完整性     └ 幂等跳过
                                    │
                            docker-local.sh up
                            （按 doctor 结果决定启动/跳过）
```

**三条设计原则**

1. **单一真相**：子系统"应该是什么样"只在清单里声明一次；脚本不在代码里内嵌参数。
2. **先探测后动作**：任何写操作之前必须先探测；探测失败（网络/认证/服务异常）**一律不写**。
3. **幂等可重入**：`reconcile` 可以在任何状态下重复执行，第二次是空操作。

## 7. 详细设计

### 7.1 清单 schema

复用生产既有字段（不新造第二套），新增两类字段：

```yaml
version: 1                      # 必填，校验器拒绝不兼容版本与未知字段
default: true                   # 是否纳入默认收敛集合

application:
  code: customer_and_opportunity
  name: 客户与商机管理系统
  description: 客户创建、审批与客户管理系统
  environment: dev
  path_prefix: /customer-opportunity
  upstream_url: http://customer-api:8090      # 容器内地址，固化在清单里，避免手敲
  public_base_url: http://localhost:8081
  client_type: confidential
  initial_admin_roles: [admin]
  allowed_service_bindings: [owner_directory_read, file_gateway_write]

runtime:                        # 沿用生产结构
  required_infrastructure_keys: [CUSTOMER_MYSQL_PASSWORD, ...]
  files:
    - path: runtime/customer.env
      template_path: subsystem-templates/customer.env.example
      compose_environment_key: CUSTOMER_RUNTIME_ENV_FILE
      required_existing_keys: [OIDC_SESSION_ENCRYPTION_KEY_BASE64]   # 首次生成后必须稳定
      generated_keys: [MACHINE_TOKEN_SECRET, FIELD_ENCRYPTION_KEY_BASE64]
      values: { ... }
  generated_keys: [...]

# —— 本方案新增 ——
onboarding_artifacts:           # 方案 D 的清理范围 + doctor 的探测范围（数据驱动）
  - OIDC_CLIENT_ID
  - OIDC_CLIENT_SECRET
  - OIDC_TENANT_ID
  - OIDC_REDIRECT_URI
  - OIDC_ROLE_CONFIG_HASH
  - PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID
  - PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID
  - PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET

preserve_keys:                  # 明确不清理（业务数据可读性相关）
  - OIDC_SESSION_ENCRYPTION_KEY_BASE64
  - MYSQL_PASSWORD
  - CUSTOMER_MYSQL_PASSWORD

gateway:
  include_path: docker/portal-apps-locations.conf
  health_path: /healthz
  compose_service: customer-api                 # 用于比对 compose 标签
```

**校验规则**（校验器实现）

1. `version` 必须为已支持版本（当前 `1`）。
2. 未知字段一律报错（防止拼写错误静默生效）。
3. `onboarding_artifacts` 与 `preserve_keys` 不允许交集。
4. `application.code` 唯一（跨文件重复即报错）。
5. `upstream_url` 必须是非回环、非 `localhost` 的容器内地址（回环只允许出现在 `public_base_url`）。
6. `path_prefix` 必须以 `/` 开头且不等于 `/`。
7. `compose_service` 必须存在于 `compose.local.yaml`（本地）/ `deploy/production/compose.yaml`（生产），且其 `com.basic-platform.path_prefix` 标签与清单一致。

### 7.2 `doctor`：只读体检

**用法**

```
subsystem.sh doctor [--env local|prod] [--format table|jsonl] [--quiet] [--fix]
```

- 默认 `--env local`、`--format table`、只读。
- `--quiet` 只输出异常项（供 `up` 内部调用）。
- `--fix` 等价于对可修复项执行 `reconcile`（见 7.4）。

**探测项（每个子系统 5 项）**

| # | 探测 | 方式 | 失败语义 |
| --- | --- | --- | --- |
| 1 | env 文件存在 | 文件系统 | `NO-ENV` |
| 2 | 接入产物齐全 | 按 `onboarding_artifacts` 检查非空且非 `REPLACE_WITH_*`/`PENDING_*` | `INCOMPLETE` |
| 3 | 凭据仍被平台接受 | `platform_catalog_token_status`（local）；生产走容器内等价探针 | `200` / `401` / `无响应` |
| 4 | 控制面有记录 | `GET /subsystem-status` | `READY` / 其它状态 / `404 无记录` |
| 5 | 网关与清单一致 | 解析 `portal-apps-locations.conf` + 比对 compose 标签 | `GATEWAY-DRIFT` |

**判定矩阵（完整）**

| env 齐全 | 凭据 | 控制面记录 | 结论 | 动作 |
| --- | --- | --- | --- | --- |
| 是 | 200 | READY 且参数一致 | `OK` | 无（幂等跳过） |
| 是 | 200 | 无记录 | `ORPHAN-CRED` | **不自动处理**，提示人工确认（凭据有效但控制面无登记，可能是跨环境复用） |
| 是 | 401 | 无记录 | `DRIFT` | 清理接入产物 → onboard |
| 是 | 401 | 有记录 | `STALE` | 清理接入产物 → onboard（控制面记录会被复用或重建） |
| 是 | 无响应 | 任意 | `UNKNOWN` | **不写**，提示先恢复平台/网关连通性 |
| 否 | — | 无记录 | `MISSING` | 直接 onboard |
| 否 | — | 有记录 | `CONTROL-ONLY` | 提示走 `/subsystem-update` 或页面"更新运行时"重新下发运行配置 |
| 否 | — | — | `NO-ENV` | 先生成 env 文件（沿用现有 `ensure_*_env_file`） |

**输出（table）**

```
子系统                    控制面        凭据      网关   结论       下一步
contract_management       READY(dev)    OK        OK     OK         —
customer_and_opportunity  无记录        401       一致   DRIFT      reconcile --env local
customer_portal           无记录        401       一致   DRIFT      reconcile --env local
project_management        无记录        未校验    一致   MISSING    reconcile --env local
data_analysis             无记录        401       一致   DRIFT      reconcile --env local
```

**退出码**：`0` 全部 OK（或 `--fix` 后全部就绪）；`1` 存在异常项；`2` 参数/前置错误（清单校验失败、凭据不可用等）。

**关键约束**：无 `--fix` 时**不得产生任何写入**（验收项）。

### 7.3 `reset_subsystem_onboarding_artifacts`：清理残留

**用法**（内部函数，由 `down --volumes` 与 `reconcile` 调用）

```
reset_subsystem_onboarding_artifacts <application_code> [--reason <drift|stale|manual>]
```

**流程**

```
1. 校验清单条目存在
2. 探测控制面（第 4 项）
   ├ 有记录  → 若 --reason drift 则拒绝（应由 update/retry 处理），报错退出
   └ 无记录  → 继续
3. 备份：cp <env_file> <env_file>.bak-<UTC timestamp>
4. 只清理 onboarding_artifacts 列出的键：写回占位（`REPLACE_WITH_*`）或置空
   （保留空行与注释结构，沿用 replace_line_in_file 的写法）
5. 绝不动 preserve_keys 中的任何键
6. 输出审计行（含 backup_path）
```

**幂等**：已清理过的键再次清理不产生差异（值相同则跳过写回）。

**不使用该函数的场景**：`--reason stale` 且控制面有记录时，仍允许清理（旧的运行面凭据确实失效），但审计里标注控制面记录将被复用。

### 7.4 `reconcile`：收敛

**用法**

```
subsystem.sh reconcile [--env local|prod] [--plan|--apply]
                       [--only <code>[,<code>...]] [--continue-on-error]
```

- `--env local`：默认 `--apply`（本地体验优先），可用 `--plan` 预览。
- `--env prod`：**默认 `--plan`**，必须显式 `--apply` 才写。
- 两者都会输出审计摘要。

**动作映射**

| doctor 结论 | 动作 |
| --- | --- |
| `OK` | 跳过 |
| `MISSING` | `onboard`（参数全部来自清单） |
| `DRIFT` | `reset ... --reason drift` → `onboard` |
| `STALE` | `reset ... --reason stale` → `onboard` |
| `CONTROL-ONLY` | 调 `/subsystem-update` 重新下发运行配置 |
| `ORPHAN-CRED` / `UNKNOWN` / `NO-ENV` | 不处理，报出并计入失败（除非 `--continue-on-error` 只报不失败） |

**幂等保证**：每次运行**先跑一遍 doctor**；只对非 `OK` 项动作；动作完成后**用同一套探测复验**，复验通过才记为成功。

**审计摘要**：人类可读表格写到 stdout；结构化 JSONL 追加到 `docker/onboarding-audit.jsonl`（本地）或由部署机指定路径（生产，建议 root-only 0600）：

```json
{"ts":"2026-09-12T02:40:11Z","actor":"ops-svc@ci","env":"prod","application_code":"customer_portal","environment":"dev","action":"onboard","before":"MISSING","after":"READY","result":"success","backup_path":null,"message":""}
```

**退出码**：`0` 收敛完成；`1` 存在失败项；`2` 前置错误；`3` 计划模式下存在待处理项（供 CI 判定"需要人工介入"）。

### 7.5 凭据提供层

统一函数 `resolve_control_plane_auth`，按优先级返回 `subsystem.sh` 的认证参数：

| 优先级 | 条件 | 传给 `subsystem.sh` |
| --- | --- | --- |
| 1 | 显式 `--cookie-file FILE` | `--cookie-file FILE` |
| 2 | 显式 `--account` + `--password-stdin` | 原样透传 |
| 3 | 环境变量 `SUBSYSTEM_ONBOARDING_ACCOUNT` + `SUBSYSTEM_ONBOARDING_PASSWORD_FILE` | `--account` + `--password-stdin`（从文件读） |
| 4 | 本地默认：`docker/.local-admin.secret`（0600，`.gitignore`） | `--account admin` + `--password-stdin` |
| 5 | 都没有 | 报错退出，并打印三种提供方式（**服务器环境不提供隐式回退**） |

**本地 secret 文件**

- 路径：`platform/docker/.local-admin.secret`
- 权限：`chmod 600`；写入时若发现权限过宽，告警并自动收紧
- 内容：仅管理员口令一行，不含用户名（用户名取自清单/参数，默认 `admin`）
- 生命周期：首次初始化成功后由 `docker-local.sh` 写入；`down --volumes` **不删除**它（否则每次重建都要人工输入），但会打印提示
- `.gitignore` 需新增：`docker/.local-admin.secret`（现有规则里 `.env.*` 不覆盖该文件名）

### 7.6 与 `docker-local.sh up` 的集成

**现状**：`up` 内部逐个子系统硬编码判断（`crm_catalog_ready` / `portal_catalog_ready` / `data_analysis_ready`），每加一个子系统就要加一段特判。

**改造**：启动前调用一次 `doctor --env local --quiet --format jsonl`，得到 ready 集合，用**数据驱动**替换逐个特判：

```
start_stack:
  1. 起 platform api + provisioner + settlement
  2. 起 unified frontend（必须在接入之前，解除互相等待）
  3. doctor --quiet  → 得到 {code: OK|DRIFT|MISSING|...}
  4. 对 OK 的子系统：按清单顺序 compose up --wait 起其服务与 Worker
     对非 OK 的：跳过，并打印一行"未就绪 + 建议执行的 reconcile 命令"
  5. verify_gateway_routes（只校验已就绪的子系统）
  6. 结尾打印接入状态一览
```

**保留兼容**：`doctor` 不可用（清单缺失、脚本降级）时，回退到现有的 `sync_*_catalog` 探测分支，保证改造期间 `up` 不会因为工具未就绪而失败。

**失败语义**：`up` 不再因为子系统未接入而失败（这是已完成止血改动的语义）；但**若所有子系统都未就绪**，`up` 仍应成功退出并打印明显的接入指引。

### 7.7 服务器部署形态

| 维度 | 本地 | 服务器 |
| --- | --- | --- |
| 清单目录 | `deploy/local/subsystems.d/` | `deploy/production/subsystems.d/`（**本期不动**） |
| 默认模式 | `--apply` | `--plan`，显式 `--apply` |
| 控制面凭据 | `docker/.local-admin.secret`（0600） | 专用运维服务账号（最小权限见 §4），口令/会话由部署环境 Secret 注入，**不落盘人类口令** |
| 审计落地 | `docker/onboarding-audit.jsonl` | 部署机 root-only 路径（0600），并随发布记录归档 |
| 触发方式 | 开发者手动 `reconcile` | 发布流程中的独立步骤（人工确认后执行 `--apply`）；**不接入自动流水线**，避免自动化改控制面 |
| 与页面/Provisioner 的关系 | 并存 | 页面仍是唯一日常入口；`reconcile` 只用于"批量重建 / 漂移修复" |

**生产启用前置条件**

1. 在本地或生产同构环境完整演练一次 `--plan` → `--apply` → 复验。
2. 服务账号创建并经权限审计（确认未授予 delete 类权限）。
3. 审计输出路径与归档方式确定。
4. 回滚演练：用 `.bak-<ts>` 恢复 + 页面"重试"路径可用。

## 8. 实施阶段与验收

| 阶段 | 内容 | 依赖 | 可验收产物 |
| --- | --- | --- | --- |
| **P1** | 清单 schema + 校验器 + `doctor` 只读；本地 secret 落盘；`.gitignore` | — | 在"平台库重建、env 残留"状态下准确报出 DRIFT；健康状态零写入 |
| **P2** | `reset_subsystem_onboarding_artifacts` + `down --volumes` 集成 | P1（探测能力） | `down --volumes && up` 不再产生 unhealthy 子系统容器 |
| **P3** | `reconcile --env local`（含审计摘要、幂等复验） | P1、P2 | 一条命令完成重新接入；连续执行两次第二次为空操作 |
| **P4** | `up` 改为数据驱动 + 回退兼容 | P1 | 新增一个子系统只需加 YAML，不改 `up` 逻辑 |
| **P5** | 生产化：`--plan` 默认、服务账号、审计归档、演练 | P3、P4 | 生产演练中零写入计划 + 显式 `--apply` 成功 + 审计可追溯 |
| **P6** | （触发式）方案 C 轮换重投影 / 方案 E 子系统降级启动 | P5 | 按触发条件单独立项 |

**每阶段通用验收**

1. `bash -n` 通过；清单校验器对 5 个真实清单全部通过。
2. `doctor` 在健康环境输出全 `OK` 且退出码 `0`。
3. 任何写操作都有审计行；计划模式零写入。
4. 新增子系统不改脚本逻辑（P4 后）。

## 9. 测试方案

仓库当前没有 shell 测试约定（无 `scripts/tests/`、无 `*_test.sh`），因此新建轻量测试目录：

```
scripts/tests/onboarding_test.sh      # 纯 bash 断言，无第三方依赖
scripts/tests/fixtures/               # 夹具 env 文件与错误清单样本
```

| 用例 | 断言 |
| --- | --- |
| 清单校验：合法清单 | 5 个真实清单 + 1 个夹具全部通过 |
| 清单校验：未知字段 / 版本不符 / 重复 code / artifacts 与 preserve 交集 / upstream 是 localhost | 逐条必须报错且信息指明字段 |
| doctor 判定矩阵 | 用夹具 env（齐全/缺项/占位符）+ 桩探针（200/401/无响应）+ 桩控制面（READY/404）覆盖 §7.2 全部分支 |
| 清理范围 | 只清 `onboarding_artifacts`；`preserve_keys` 与数据库密码逐字节不变；已备份文件存在 |
| 幂等 | 连续两次 `reconcile`，第二次零写入（用文件 mtime/hash 断言） |
| 计划模式 | `--env prod` 不带 `--apply` 时，所有 env 文件 hash 不变 |
| 审计 | 每个动作产生一行合法 JSONL，字段齐全 |

**端到端演练**（在本地执行，作为 P2/P3 的验收）

```
1. docker-local.sh down --volumes      # 制造根因场景
2. 确认各子系统 env 仍持有旧凭据（清理是否生效）
3. docker-local.sh up                  # 必须以 0 退出，且无 unhealthy 子系统
4. subsystem.sh doctor                 # 必须报出 DRIFT/MISSING
5. subsystem.sh reconcile --env local  # 必须完成接入
6. subsystem.sh doctor                 # 必须全 OK
7. docker-local.sh up                  # 子系统服务必须真实起来并通过网关校验
```

## 10. 文件级改动清单

| 文件 | 类型 | 改动 |
| --- | --- | --- |
| `deploy/local/subsystems.d/{contract_management,customer_and_opportunity,customer_portal,project_management,data_analysis}-local.yaml` | 新增 | 5 个本地清单（含 `onboarding_artifacts` / `preserve_keys` / `gateway`） |
| `platform/scripts/lib/onboarding-manifest.sh` | 新增 | 清单解析与校验（§7.1 规则 1–7） |
| `platform/scripts/lib/onboarding-doctor.sh` | 新增 | 探测与判定矩阵（§7.2） |
| `platform/scripts/lib/onboarding-reconcile.sh` | 新增 | 动作映射、清理、审计、幂等复验（§7.3/7.4） |
| `platform/scripts/lib/onboarding-auth.sh` | 新增 | 凭据提供层（§7.5） |
| `platform/scripts/subsystem.sh` | 修改 | 新增 `doctor` / `reconcile` 子命令；把 `ensure_authenticated` 改为调用凭据提供层 |
| `platform/scripts/docker-local.sh` | 修改 | `down --volumes` 调清理；`up` 改为 doctor 驱动 + 回退兼容；首次初始化写 secret 文件 |
| `platform/.gitignore` | 修改 | 新增 `docker/.local-admin.secret`、`docker/*.bak-*` |
| `platform/scripts/tests/onboarding_test.sh` + `fixtures/` | 新增 | §9 用例 |
| `platform/docs/local-onboarding-hardening.md` | 新增 | 面向使用者的命令手册（与本方案分离） |
| `platform/docs/README.md` | 修改 | 索引补两行 |

> 脚本落点说明：现有 `scripts/` 下是扁平结构，本方案引入 `scripts/lib/` 存放可被 `subsystem.sh` 与 `docker-local.sh` 共同 source 的实现，避免逻辑重复。若希望保持扁平，可改为 `scripts/onboarding-*.sh` 前缀。

## 11. 风险与回滚

| 风险 | 缓解 |
| --- | --- |
| 误清有效凭据（把"探测失败"当成"无记录"） | 判定矩阵里 `UNKNOWN`（无响应）**一律不写**；只有"探测成功且控制面明确 404"才允许清理；清理前强制备份 |
| 自动化在生产误写控制面 | `--env prod` 默认 `--plan`；服务账号不授予 delete 权限；`--apply` 需显式；审计留痕 |
| 清单与真实编排漂移 | 校验规则 7 比对 `compose.local.yaml` 的 `com.basic-platform.*` 标签；不一致直接拒绝 |
| 本地 secret 文件泄露 | `chmod 600` + `.gitignore` + 写入时权限自检；CI 增加 Secret 扫描 |
| `up` 因工具未就绪而失败 | 保留回退分支（§7.6"保留兼容"） |
| 清理后忘记重新接入 | `up` 结尾必然打印接入状态一览与建议命令；退出码仍为 0（不阻断，但可见） |

**回滚**：清理留 `.bak-<timestamp>`；`reconcile` 的写操作全部可逆（重新 onboard 会重建控制面记录，或经页面"重试"）；生产启用前完成演练。

## 12. 未纳入范围

| 项 | 理由 | 触发条件 |
| --- | --- | --- |
| 方案 C：平台库作为唯一真相（轮换 + 重投影） | 改动面最大，涉及 provisioner 收敛与轮换时机；且决策 4 要求不与"消除不一致"混做。**关键约束**：OAuth secret 不可回读（有测试断言），只能轮换重发 | 方案 B 稳定运行一个发布周期后 |
| 方案 E：子系统目录同步失败降级启动 | 需改 4 个子系统仓库 | 生产出现"子系统目录同步失败导致整套发布回滚" |

## 13. 安全项：一处既有问题（建议一并处理）

`contract_management/.env.local` **被 git 跟踪**，且含真实 `OIDC_CLIENT_SECRET`、`OIDC_CLIENT_SECRET_ROLLBACK` 等凭据（本次核实）。平台仓库的 `.gitignore` 已忽略 `docker/.env.local`，但该文件属于另一个仓库，其忽略规则不覆盖它。

建议：将其移出跟踪（`git rm --cached`）+ 补该仓库 `.gitignore` + 以 `.env.example` 模板代替。**这会改变该仓库的既有约定，需单独确认后再做。**

## 14. 附：已完成的止血改动

以下改动已实现并推送，**不替代本方案**，只是让 `up` 不再因为未接入而整体失败：

- `up` 先启动统一前端，解除"接入需要网关、网关等待接入"的互相等待。
- CRM / Portal 的授权目录发布失败不再终止整套 `up`，改为标记未就绪、跳过其 API 与 Worker，并打印可执行接入命令。
- 数据看板：新增启动前令牌探针（`platform_catalog_token_status`），凭据失效时跳过看板块，避免 `dashboard-api` 重启循环。
- `verify_gateway_routes` 的三处健康检查按各子系统就绪开关跳过，避免"已跳过启动却仍去校验"的二次失败。
