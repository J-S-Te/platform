# 受控商业运行时组件登记

> 源码边界说明；发布与真实 READY/ACK 验收须另行核对运行证据。

`RuntimeLicenseEnrollmentService` 由已认证、已授权的受控部署任务调用，不是浏览器可直接填组件的登记端点。组合根 `internal/bootstrap/api.go` 注入 `BuiltinRuntimeComponentCatalog`、普通 OAuth Client 管理器和协调器，实例/商业环境来自持久化设置。只有 `ErrRuntimeLicenseNotInitialized` 保留从未初始化实例的旧兼容路径，其余读取、登记和交付错误均明确阻断。

## 当前标准路径：内置组件目录

`PrepareWithSettings` 在存在目录源时，对新安装及持久化的 `LEGACY_IMAGE_V1`、`BRIDGING_COMPONENT_V2`、`BUILTIN_COMPONENT_V2` 都走 `prepareBuiltin`，旧集合通过 `ReconcileBuiltinComponents` 受控桥接，不要求每次重建重新提供镜像批准。登记协议为 2，`license-core/catalog.Version` 当前为 1；签名快照协议和商业许可证 JWS 协议仍为 1，不能统称“快照 V2”。

目录编译进平台、Agent 和消费者，拒绝未知、重复、跨应用组件或遗漏必需消费者。基础设施（当前 Metabase）不领取业务凭据、不产生业务 ACK；条件组件只在选定运行拓扑需要时纳入。不能从仓库二进制数量推断生产启用了多少消费者。

V2 Client ID 绑定 `instance/application/environment/service/credential_generation`，明确排除 image/coverage/release digest，普通镜像重建不改变组件身份。每组件独立 service OAuth Client、`client_credentials`、唯一 `license.runtime` scope、300 秒 Token。注册/协调成功后才轮换并交付 Secret；跨服务部分失败可重试，不冒充控制面与 Agent 的全局事务。

本地 Agent 的 `deliverLocalBuiltinRuntimeLicense` 使用当前内置目录与实际 Compose 镜像，写入每组件私有 `runtime/license-<service>.env`（0600），固定当前不可变镜像并挂载独立持久状态和平台公钥；不从业务共享 env 复用 API/Worker 凭据。目录代际变化、成员变更及从 V1 桥接均由协调器重新确认，不能删除状态卷回到兼容路径。

## 遗留镜像批准路径

未使用内置目录的遗留批准源仍保留：生产 `runtime-license-<application>-<environment>.json`、本地 `runtime-approvals/local-<application>-dev.json`，核对审查标志、固定服务集合、不可变 image/coverage digest、文件权限及当前源码/Compose。V1 新发布通过受控 CAS 与完整 release 核验，移除成员需显式批准，保留退役身份墓碑。该路径的存在不表示所有当前环境都必须使用 V1 批准。

当前本地脚本 `docker-local.sh` 的固定记录预检仍要求 `local-*-dev.json` 覆盖固定服务，而 V2 Agent 可不读取此文件登记/交付。纯 V2 且无遗留批准的普通启动可能受此兼容差异阻断；本轮仅静态发现，未运行启动验证。不能用手工删固定记录、补假批准或清状态绕过。

## 操作与成功条件

管理员从既有接入/更新/重试流程恢复环境，在[商业授权页面](../../../../../sharedDocs/09_商业授权/README.md)检查和激活，页面没有任意组件编辑器。运行组件登记、部署 READY、有效签名许可证、ENFORCED 策略与当前修订 READY/ACK 是不同事实。

成功至少要求部署结果、`snapshot_current=true`、当前完整消费者集合及 `issued_revision=ack_revision=应用 revision`，同时具备准备/确认时间；保留 ENFORCED 的更新也需重新证明当前集合。首次切至 ENFORCED 会再推进修订并要求确认。失败读取模式、修订、诊断和受控 Agent 状态，修复后沿原操作恢复，不共享凭据、伪造 ACK 或退回旧模式。

源码入口：`runtime_license_enrollment.go` → `infrastructure/runtime_component_catalog.go` → `license-core/catalog/catalog.go` → `license/coordination`；交付分别见 `infrastructure/local_runtime_license.go`、`runtime_license_delivery.go`。商业 instance/customer/environment 与应用目录租户/应用/环境 ID 是不同边界。

## 统一接入向导与独立 Worker

生产向导与后台候选基线使用同一内置 V2 目录，不再把遗留 V1 镜像批准文件当作 V2 登记前提。生产 Agent 从受审目标的实际 Compose 拓扑读取不可变镜像及本机存在性（含迁移/初始化镜像），只返回服务与镜像投影；空清单、不可验证或缺失镜像不能视为就绪。候选摘要绑定清单、组件目录代次及实际镜像。缺少一致性事实时不能返回“无需操作”。本地遗留批准预检兼容边界仍见上文。

统一向导任务由独立 platform-worker 执行，不能依赖仅在 HTTP Handler 配置的内存登记服务。Worker 使用相同持久化商业实例、厂商信任公钥、平台受管签名身份和协调器，在 Agent 调用前完成 V2 登记；按 Agent 报告的已选消费者收窄条件组件，并校验必需组件完整。登记失败阻止部署，凭据仅通过受限 Agent 接口交付，不进入阶段证据。本地模式沿用真实确认等待；生产运行确认与强制执行仍以商业授权页面的当前修订 READY/ACK 为准，部署成功不代表所有组件已经 ACK。

V2 免读旧退役文件的证明由协调器内部查询完整成员历史生成，绑定应用修订；公开 Status 只含活跃成员，不能作为无退役需求的证明。首次登记或完整历史仍全部包含于当前部署选集时可免读 V1 退役文件；存在已移除历史成员、历史读取失败或修订冲突时保留原退役门禁。失败重试不能因旧成员已写墓碑而变成免审更新。
