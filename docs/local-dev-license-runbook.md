# 本地开发：商业授权与子系统接入 Runbook

> 源码校准：2026-10-22。适用于 `basic-platform-local` 的 dev 编排；当前脚本、登记服务、Agent 和消费者按源码核对。本地预检回归见 `.ai/qa/docs-gap-fixes-20261022/`，未启动容器、导入许可证、迁移或验证真实业务。
> 生产发布及升级批准使用[生产部署入口](../deploy/production/README.md)和[商业授权教程](../../sharedDocs/09_商业授权/README.md)，不能照搬本地固定文件。

## 1. 先确认实例和登记模式

### 1.1 实例初始化与组件登记不同

持久化的商业实例设置来自许可证服务，未初始化时返回 `ErrRuntimeLicenseNotInitialized`，受控接入保留从未登记旧部署的兼容路径。实例已初始化后，读取/登记/交付错误失败关闭。首个应用登记不是“创建实例”的等价操作；不得删除实例或消费者状态恢复兼容。

### 1.2 当前标准登记使用内置目录

API 组合根注入 `BuiltinRuntimeComponentCatalog`。`PrepareWithSettings` 对新安装及 V1/桥接/V2 既有应用走内置目录并在必要时调用 `ReconcileBuiltinComponents`，不是每次构建都要求新的 image/coverage 批准。

- `registration_protocol=2`；catalog version 当前为 1；签名快照/JWS 协议仍为 1。
- 组件身份由实例、应用、商业环境、service ID 和凭据代际决定，普通镜像变化不改变 V2 Client ID。
- 每组件独立凭据及持久状态；基础设施 Metabase 不产生业务 ACK；条件组件以实际选择拓扑为准。
- `LEGACY_IMAGE_V1/BRIDGING_COMPONENT_V2/BUILTIN_COMPONENT_V2` 是持久化模式，不能手工改库切换。

### 1.3 本地预检按组件识别 V1/V2

`prepare-license`、`refresh-approvals`、`recover-license-pins` 保留维护 V1 本地批准/固定记录的能力。正式批准位于 `runtime-approvals/local-<application>-dev.json`（0600），候选为 `.candidate.json`，不能改名替代审查。

`docker-local.sh` 对 V1 固定服务继续核对匹配的 legacy 批准；对 V2 读取 `runtime/license-<本地服务>.env`，要求 Agent 固定路径、普通文件、`0600` 权限、协议 2/目录版本 1、实例商业环境、服务绑定和受控挂载，不因缺旧批准拒绝启动。服务别名按本地 Compose 对应逻辑 service ID 检查；应用必需消费者及迁移镜像必须完整，条件组件缺席不会被自动启用。实例商业环境可以是 `production`，与应用接入环境 `dev` 不等同；状态文件须与 Agent 按实例、商业环境、Client、服务和目录代际的派生结果一致，额外可覆盖运行绑定的 Compose 字段会被拒绝。两种模式都要有已固定的不可变镜像，混合模式中的 V1 组件仍受原批准约束。

预检只读、不执行 env 文件、不输出 Client Secret，也不赋予登记或商业权益。它通过后，实际授权凭据、实例绑定、商业许可证和当前自然 ACK 仍由平台/消费者核验。不要删除固定记录、状态或伪造批准恢复。

## 2. 启动与构建

从 platform 目录执行已授权的本地操作。先确认 Docker 可达、当前 context 对应正确测试环境及配置与既有卷匹配；不强制所有开发者使用同一个 context。

| 入口 | 当前作用 |
| --- | --- |
| `bash scripts/docker-local.sh status` | 状态与下一步提示 |
| `bash scripts/docker-local.sh dev-up` | 控制面启动、迁移/首个管理员/探测候选和批准候选核对单；不自动批准 |
| `bash scripts/docker-local.sh up` | 已固定服务不重建，只补缺失的未登记本地镜像；仍执行配置、固定镜像预检、迁移和分阶段启动 |
| `bash scripts/docker-local.sh up --build` | 准备新候选镜像，不自动替换已固定运行镜像，不发布批准 |
| `bash scripts/docker-local.sh build-images` | 只构建候选，不检查旧运行镜像、不迁移、不启动、不改固定记录 |
| `bash scripts/docker-local.sh up --core-only` | 恢复平台依赖、平台迁移/API/Agent、文件网关、Keycloak、统一前端；不代表业务系统就绪 |
| `SKIP_IMAGE_PREPARATION=1 bash scripts/docker-local.sh up` | 跳过基础镜像拉取/构建，存在性与固定记录预检仍生效 |

`restart` 也走启动链，不是纯重启。`ps/logs/verify/config` 的包装环境准备可能补齐本地文件，严格只读审计不能直接调用。脚本详情见[本地 Docker 操作](local-docker-operations.md)。

首次管理员优先交互输入，自动化用 `--admin-password-stdin`；账号名/显示名可用对应参数，不把密码放 argv。初始化后保留账号保护，不能删库重建解锁。

## 3. 接入与受控更新

优先从“应用接入 → 统一接入向导”检查目标应用/环境，动作由服务端决定；目录登记、认证、部署、商业授权及业务验证分项看待。旧目录/环境保留，不反复 onboard。

CLI 与页面统一入口：

```bash
bash scripts/subsystem.sh connect --application-code customer_and_opportunity --environment dev
bash scripts/subsystem.sh connect --resume <操作ID>
```

旧部署可沿原记录使用受控 `update/retry`。若显式传 `--issuer-alias`，必须与已登记环境一致；不要把所有环境写死为 keycloak。管理员密码从交互/标准输入或受控 Cookie 文件读取，不能放命令行。

V2 Agent 会读取真实 Compose 镜像、交付组件私有 env 并写固定记录；部署后等待真实 READY 和当前 ACK。业务 Worker 在权益无效时暂停领取，进程存活不是业务可用。普通镜像重建仍须受控部署采用新镜像，不能只构建后期望现有固定记录自动更新。

## 4. 遗留本地批准与固定记录恢复

以下只针对仍使用 legacy 批准或旧脚本固定记录恢复的环境，不是所有 V2 首次登记的前置流程。先核实当前模式、目标版本和实际阻断原因。

```bash
bash scripts/docker-local.sh prepare-license --application <应用编码> --operator <操作者标识>
```

候选生成会写独立候选文件，但不覆盖正式批准、不迁移或启动。核对全部消费者、迁移镜像、实际不可变 ID、源码/Compose 指纹及协议后，显式发布：

```bash
bash scripts/docker-local.sh prepare-license --application <应用编码> --operator <操作者标识> --approve
```

已有正式批准且内容不同才加 `--replace`；候选不能改名。多应用可用 `refresh-approvals --operator <标识>` 生成核对单，加 `--approve` 才发布；它会逐应用调用工具，不能承诺跨文件全局事务。

停机且已确认有效批准/镜像完整时，可运行 `recover-license-pins` 预览，再经审核加 `--apply`。该入口先备份再原子替换固定文件，不改运行凭据/状态、不启动/构建；拒绝运行中项目或不安全输入。恢复顺序为候选构建 → 必要时仅控制面启动 → 审核有效批准 → 受控更新 → 普通启动。不要重复构建、批量删除卷或用可变标签填补固定 ID。

## 5. 错误与成功判断

### 5.1 获取诊断

先读取平台脱敏部署状态、操作 ID/request_id、阶段、generation 和稳定错误码。平台和 provisioner 的日志是补充证据；分享前去掉凭据、Token、完整环境和业务正文。不要打印所有消费者状态 JSON 或把密码放 `docker exec` argv。

### 5.2 常见错误

| 现象 | 核查 |
| --- | --- |
| LICENSE_RUNTIME_DOCKER_UNAVAILABLE | Docker/受限代理不可达；恢复依赖，不绕过校验 |
| LICENSE_RUNTIME_APPROVAL_UNAVAILABLE / MISMATCH | 核对登记模式及是否走 legacy 批准、文件权限/指纹/镜像集合；不是所有错误都等于“无批准文件” |
| LICENSE_RUNTIME_STARTUP_FAILED | 组件实际启动或准备/确认失败，按服务诊断处理 |
| 固定镜像缺失/固定服务无对应批准 | 区分真实镜像缺失与 V2/legacy 预检兼容差异；保留状态并受控恢复 |
| IAM_* 校验/状态冲突 | 核对目标目录、issuer alias、version 和认证观察期，不直接改数据库 READY |
| COMMERCIAL_LICENSE_DENIED | 按下节核对许可证和组件状态，不扩权或清状态 |

### 5.3 验收层次

`202` 是受理，部署 READY 是本轮运行操作完成；还需有效的本实例许可证、正确应用权益和日期、`snapshot_current=true`、完整当前消费者集合、`issued_revision=ack_revision=应用 revision` 及准备/确认时间。首次 ENFORCED 会再推进修订，等待新一轮确认。最后执行真实 OIDC/Broker 登录与典型业务权限验证，不能只数固定文件里镜像 ID 出现次数。

### 5.4 业务 403 与许可证

先区分身份/范围拒绝、依赖不可用和 `COMMERCIAL_LICENSE_DENIED`。消费者 `ESSENTIAL_SERVICE` 保持可用；业务读/导出/写根据签名许可证及绝对时间判定，不是所有 HTTP 一律拒绝。只有经签名快照明确认可的存量迁移资格，在从未观察到商业许可证且未开始强制执行时保留旧兼容；本地新登记不自动获得此资格。

恢复通过本实例登记、有效厂商签发产物导入/激活和真实 READY/ACK。客户运行环境不持有厂商私钥；禁止删除 `/var/lib/commercial-license`、关闭 `COMMERCIAL_LICENSE_ENABLED`、伪造 ACK 或倒退模式绕过门禁。签发/续签交由[厂商签发流程](../../sharedDocs/09_商业授权/README.md)。

### 5.5 导入冲突与取回续签基准

- `LICENSE_VERSION_NOT_INCREASING`：版本不高于已记录版本，需厂商更高版本，刷新无效。
- `LICENSE_BINDING_MISMATCH`：客户/实例/环境绑定不符，不能在此实例强行导入。
- 并发冲突：重新预览，再基于当前 revision 提交。

具有 `platform:license:read` 的管理员可从 `GET /api/v1/licenses/artifact` 取回 current/pending 原始 JWS 和 `highest_version`，使用平台受控会话；不把机器 runtime Token 当管理员凭据。产物须保存到受控目录并交给签发人员核验，不能在日志、聊天或公共临时文件展示。数据库及签名材料需要一致备份；数据库丢失不能通过本地改实例 ID 复用旧资格。

## 6. 源码依据与未验证范围

- 组合根：`internal/bootstrap/api.go`；登记/桥接：`applicationregistry/application/runtime_license_enrollment.go`。
- 内置目录：`applicationregistry/infrastructure/runtime_component_catalog.go`、`license-core/catalog/catalog.go`。
- 本地交付：`applicationregistry/infrastructure/local_runtime_license.go`；启动/预检：`scripts/docker-local.sh`。
- legacy 工具：`prepare-local-runtime-license.py`、`refresh-local-runtime-approvals.py`、`recover-local-license-pins.py`。
- 消费端：`license-core/consumer/consumer.go`、`license-core/runtime/state.go`。

旧 2026-10-15 排障中的现场 Docker context、日志、固定镜像和测试结果均不作当前环境事实。本轮未访问 Docker、业务库、签发私钥或真实许可证；V2 与 legacy 脚本兼容、全组件进程确认和浏览器业务验收仍需隔离环境验证。

## 7. 运行时护栏（2026-10-24 增补）

以下行为已在 `scripts/docker-local.sh` 与 `scripts/local-status.py` 落地，目的是把"事后 403 / 静默跑旧镜像"提前成启动期的明确错误。

### 7.1 `refresh-*` 不再重建已固定的服务

已登记应用的运行镜像由固定记录（`runtime/local-license.compose.json`）按不可变 ID 绑定。`refresh-<app>-api` 在构建前会调用 `refuse_pinned_rebuild`，命中固定记录时**直接拒绝**并打印受控路径：

    build-images → refresh-approvals --operator <标识> → --approve
      → 应用接入页受控更新（或 subsystem.sh retry --application-code <编码>）→ up

原因：重建会产生新镜像 ID，使固定记录与批准同时失效；已运行容器仍跑旧镜像，而 `up` 预检会因 `No such image` 阻断。

### 7.2 刷新流程先过固定记录预检

所有 `refresh-*` 在构建前执行 `preflight_local_license_images`：固定镜像缺失、批准指针失效、组件清单不一致都会在**迁移与重启之前**一次性报出。

### 7.3 构建后提示与迁移镜像滞后检测

`report_candidate_drift` 在刷新结束时：

- 列出"刚构建的候选镜像与固定记录不一致"的服务（提示新构建不会生效）；
- 当应用镜像比其迁移镜像更新时告警——这正是 `migrations are current` 假阳性的根因（迁移镜像内嵌的迁移集合落后于应用）。

### 7.4 `local-status` 新增判定

| 检查 | 判定 |
|---|---|
| 授权实例 | 需同时满足 `license_deployment > 0` 且 `license_artifact > 0`；无产物时明确提示"业务会返回 COMMERCIAL_LICENSE_DENIED" |
| 授权登记覆盖 | `license_runtime_member` 的应用数对比组件清单；不足时提示重新同步缺失应用 |
| 许可证有效期 | 解码产物载荷，显示最早到期天数 |
| 运行镜像一致 | 逐个容器比对实际镜像与固定记录；偏离时提示走批准 + 受控更新 |

### 7.5 项目后台状态自愈

`runtime/project-background/operation.json` 停在 `activating/prepared/teardown-drained` 且后台容器已随整体停机消失时，`up` 会先把状态文件**归档**（`*.stale.<UTC时间>`，保留证据）再重新准备，避免"无法重新证明排空"导致整栈起不来。运行中或容器仍存在时不做任何处理。
