# Basic Platform 文档索引

> 源码校准：2026-10-15（新增本地开发商业授权与子系统接入 Runbook）；2026-10-23 增补 MySQL binlog 减量与备份位点方案索引（减量未执行；位点方案已实施、未部署）。当前操作手册与带日期历史记录分开使用；本轮不宣称生产验收完成。

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| [全系统操作手册导航](../../sharedDocs/10_操作手册/README.md) | 管理员与业务人员 | 按角色、前置条件、操作、结果和异常处理使用各系统 |
| [操作文档核验索引](../../sharedDocs/操作文档核验索引.md) | 文档维护、研发、验收人员 | 本次更新范围、源码链路证据、历史材料与验证边界 |
| [File Gateway 生产运行手册](./file-gateway-production.md) | 文件服务与部署运维 | 本地持久化目录、v2 接入、备份对账与恢复 |
| [商业授权教程](../../sharedDocs/09_商业授权/README.md) | 平台管理员、厂商签发人员 | 安装授权、组件批准、运行激活与离线工具 |
| [子系统开发与统一身份接入手册](./subsystem-onboarding.md) | 子系统开发、联调、平台管理员 | 项目契约、OIDC、首次接入、部署 Agent、状态查询、失败重试、验收、更新、网关、目录同步、下线与排障 |
| [本地 Docker 与脚本使用说明](./local-docker-operations.md) | 开发、测试、运维 | Docker 启动、环境文件、LAN、定向刷新、测试数据和网关锁 |
| [本地开发商业授权与子系统接入 Runbook](./local-dev-license-runbook.md) | 开发、测试、运维 | 授权实例初始化开关、批准文件、镜像 ID 冻结、错误码对照表与逐条处置 |
| [生产环境 CI/CD 部署](../deploy/production/README.md) | 生产运维、发布人员 | 服务器初始化、GitHub Environment、上线顺序、生产安全边界与恢复 |
| [Keycloak 生产运维 Runbook](./keycloak-production-operations.md) | IAM/平台运维、值班人员 | 凭据、备份恢复、健康检查、HA、监控告警和灾备演练 |
| [MySQL binlog 减量与保留方案](./operations/mysql-binlog-volume-plan.md) | 数据库/平台运维 | `platform-mysql` 与 `keycloak-db` 的只读核对、分阶段参数调整、回滚与验证（2026-10-23 编制，未执行） |
| [MySQL 备份 binlog 位点方案](./operations/mysql-backup-binlog-position-plan.md) | 数据库/平台运维、发布人员 | 为 `platform-mysql` 与 `keycloak-db` 的逻辑备份补记位点、sidecar、失败策略与重放验证（已实施并本地验证，未部署） |
| [外部客户绑定机器契约](./external-customer-binding-contract.md) | CRM、Portal、平台开发与联调 | 客户绑定、解绑、幂等、防重放、授权上下文和迁移边界 |
| [Keycloak V2 切换执行基线](./keycloak-v2-execution.md) | IAM/平台运维、发布人员 | 分应用、分环境切换、观察期、回滚窗口和旧认证退役 |
| [Keycloak Broker 隔离记录](./operations/keycloak-broker-isolation.md) | IAM/平台运维、应用联调 | 平台与客户 Portal 的 Broker、Client、Realm 与 claims 隔离 |
| [后端子系统解耦计划](./backend-subsystem-decoupling-plan.md) | 平台架构、后端开发 | 应用清单化、Provisioner 边界与迁移策略 |
| [本地子系统接入可靠化改造方案](./local-onboarding-hardening-plan.md) | 开发联调、脚本维护、架构评审、生产部署 | 重新部署时的接入漂移根因、doctor 体检、reconcile 收敛、按环境切换凭据、实施阶段与验收（2026-09-12 历史方案，当前入口见接入/本地运行手册） |
| [平台治理能力审计与整改方案](./platform-governance-audit-and-remediation-plan.md) | 架构、安全、各子系统负责人、测试 | 登录管控/权限赋权、站内通知、集中审计、外网 IP、并发控制五项需求的实现差距与三阶段整改路线（2026-09-12 静态审计快照，不作当前缺陷清单） |
| [平台治理整改方案（轻量化 · 企业级）](./governance-remediation-lite-plan.md) | 架构、各子系统负责人、发布负责人 | 保留 2026-09-12 四个工作包与取舍，按历史方案阅读，当前实现回到操作/能力文档核对 |
| [Go 代码注释规范](../../sharedDocs/04_后端开发/Go代码注释规范.md) | Go 开发、代码审查 | 中文说明性注释、导出符号、关键边界和文档同步要求 |

## 最重要的操作原则

首次管理员初始化出现岗位不可用时，参见[首次管理员岗位修复](./operations/bootstrap-position-repair-2026-09-08.md)，使用新增迁移恢复，不删除数据库或改写历史迁移。

1. `docker-local.sh up` 不要求重建已有 `.env.local`。
2. 子系统首次接入只执行一次；日常发布不执行 onboard/offboard。
3. 局域网 OAuth HTTP 回调可按平台策略启用；管理员凭据访问非回环 HTTP API必须单独显式放行。
4. Secret 不进入 Git、命令行参数或普通日志。
5. 永久下线前必须完成数据保留、会话处置和恢复预案。
6. 本地编排使用 `compose.local.yaml`，生产编排使用 `deploy/production/compose.yaml`；离线包使用安装/升级脚本管理同一生产边界，不得提交任何实际 `.env`。

## 历史实现记录（2026-08-27，非当前操作结论）

- OAuth Client 管理仓储已经同时支持按内部 ID 和 `client_id` 查询；API 与 Worker 共用同一应用层接口，避免构建时因接口漂移失败。
- 平台内部登录与客户 Portal 使用不同的 Keycloak Broker alias、OAuth Client 和应用/环境授权上下文；不得恢复共用 alias 的旧配置。
- 生产子系统接入由审核清单和 `subsystem-provisioner` 执行，首次管理员授权、失败重试和状态查询均以页面返回的 `request_id` 关联日志；修复后应重试既有环境，不重复创建。
- 生产镜像必须使用 `image@sha256:digest`。CI 构建失败时先核对平台 API 与 provisioner 是否来自同一版本，再检查 Compose 展开结果。
