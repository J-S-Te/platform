# Basic Platform 文档索引

> 更新日期：2026-08-27

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| [子系统开发与统一身份接入手册](./subsystem-onboarding.md) | 子系统开发、联调、平台管理员 | 项目契约、OIDC、首次接入、部署 Agent、状态查询、失败重试、验收、更新、网关、目录同步、下线与排障 |
| [本地 Docker 与脚本使用说明](./local-docker-operations.md) | 开发、测试、运维 | Docker 启动、环境文件、LAN、定向刷新、测试数据和网关锁 |
| [生产环境 CI/CD 部署](../deploy/production/README.md) | 生产运维、发布人员 | 服务器初始化、GitHub Environment、上线顺序、生产安全边界与恢复 |
| [Keycloak 生产运维 Runbook](./keycloak-production-operations.md) | IAM/平台运维、值班人员 | 凭据、备份恢复、健康检查、HA、监控告警和灾备演练 |
| [外部客户绑定机器契约](./external-customer-binding-contract.md) | CRM、Portal、平台开发与联调 | 客户绑定、解绑、幂等、防重放、授权上下文和迁移边界 |
| [Keycloak V2 切换执行基线](./keycloak-v2-execution.md) | IAM/平台运维、发布人员 | 分应用、分环境切换、观察期、回滚窗口和旧认证退役 |
| [Keycloak Broker 隔离记录](./operations/keycloak-broker-isolation.md) | IAM/平台运维、应用联调 | 平台与客户 Portal 的 Broker、Client、Realm 与 claims 隔离 |
| [后端子系统解耦计划](./backend-subsystem-decoupling-plan.md) | 平台架构、后端开发 | 应用清单化、Provisioner 边界与迁移策略 |
| [Go 代码注释规范](../../sharedDocs/04_后端开发/Go代码注释规范.md) | Go 开发、代码审查 | 中文说明性注释、导出符号、关键边界和文档同步要求 |

## 最重要的操作原则

1. `docker-local.sh up` 不要求重建已有 `.env.local`。
2. 子系统首次接入只执行一次；日常发布不执行 onboard/offboard。
3. 局域网 OAuth HTTP 回调可按平台策略启用；管理员凭据访问非回环 HTTP API必须单独显式放行。
4. Secret 不进入 Git、命令行参数或普通日志。
5. 永久下线前必须完成数据保留、会话处置和恢复预案。
6. 本地只使用 `compose.local.yaml`，生产只使用 `deploy/production/compose.yaml`；不得提交任何实际 `.env`。

## 近期实现基线（2026-08-27）

- OAuth Client 管理仓储已经同时支持按内部 ID 和 `client_id` 查询；API 与 Worker 共用同一应用层接口，避免构建时因接口漂移失败。
- 平台内部登录与客户 Portal 使用不同的 Keycloak Broker alias、OAuth Client 和应用/环境授权上下文；不得恢复共用 alias 的旧配置。
- 生产子系统接入由审核清单和 `subsystem-provisioner` 执行，首次管理员授权、失败重试和状态查询均以页面返回的 `request_id` 关联日志；修复后应重试既有环境，不重复创建。
- 生产镜像必须使用 `image@sha256:digest`。CI 构建失败时先核对平台 API 与 provisioner 是否来自同一版本，再检查 Compose 展开结果。
