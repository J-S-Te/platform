# Basic Platform 文档索引

> 更新日期：2026-08-24

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| [子系统开发与统一身份接入手册](./subsystem-onboarding.md) | 子系统开发、联调、平台管理员 | 项目契约、OIDC、首次接入、部署 Agent、状态查询、失败重试、验收、更新、网关、目录同步、下线与排障 |
| [本地 Docker 与脚本使用说明](./local-docker-operations.md) | 开发、测试、运维 | Docker 启动、环境文件、LAN、定向刷新、测试数据和网关锁 |
| [生产环境 CI/CD 部署](../deploy/production/README.md) | 生产运维、发布人员 | 服务器初始化、GitHub Environment、上线顺序、生产安全边界与恢复 |
| [Keycloak 生产运维 Runbook](./keycloak-production-operations.md) | IAM/平台运维、值班人员 | 凭据、备份恢复、健康检查、HA、监控告警和灾备演练 |
| [Go 代码注释规范](../../sharedDocs/04_后端开发/Go代码注释规范.md) | Go 开发、代码审查 | 中文说明性注释、导出符号、关键边界和文档同步要求 |

## 最重要的操作原则

1. `docker-local.sh up` 不要求重建已有 `.env.local`。
2. 子系统首次接入只执行一次；日常发布不执行 onboard/offboard。
3. 局域网 OAuth HTTP 回调可按平台策略启用；管理员凭据访问非回环 HTTP API必须单独显式放行。
4. Secret 不进入 Git、命令行参数或普通日志。
5. 永久下线前必须完成数据保留、会话处置和恢复预案。
6. 本地只使用 `compose.local.yaml`，生产只使用 `deploy/production/compose.yaml`；不得提交任何实际 `.env`。
