# 192.168.31.153 新机构建与离线部署验收报告

## 结论状态

**部分通过；完整全系统验收被目标机内存容量阻塞。**

基础平台、统一前端、客户与商机、客户自助门户、合同、项目和结算已在目标机完成真实导入、受控接入、启动与健康验收。数据分析系统的 API 和 Worker 能启动，但 Metabase 在仅约 3.3 GiB 物理内存、swap 已耗尽的目标机上无法在健康时限内完成初始化，因此已使用保留数据的停用流程恢复服务器，不将其记为通过。

本报告只记录 2026-09-27 至 2026-09-28 在目标机重新取得的证据。历史构建产物、旧服务器结果、脚本替身测试和文档推演不计入目标机通过结论。

## 验收边界

- 目标机：`192.168.31.153`，SSH 端口 22。
- 第一阶段作为联网构建机；第二阶段使用独立交付目录验证离线包安装和分阶段部署。
- 禁止重启宿主机、修改 root 密码、删除既有数据或操作 `192.168.3.38`。
- 认证口令、Token 和接入 Secret 未写入源码、命令参数、手册、证据或日志。
- 未执行宿主机重启、长期稳定性观察和生产数据恢复；这些项目不在本报告中宣称通过。

## 环境证据

- Ubuntu 26.04.1 LTS，Linux x86_64，4 vCPU。
- 内存约 3.3 GiB，swap 约 3.8 GiB；数据分析启动期间 swap 全部耗尽。
- 根分区约 97 GiB，预检时约 85 GiB 可用，inode 充足。
- Docker Engine 29.7.2、Compose 5.5.0、Buildx 0.36.1。
- 初始状态未发现容器、命名卷、自定义 Docker 网络或 `/opt/unified-identity-platform`。
- NTP 已同步；详细脱敏预检见同目录 `PREFLIGHT.md`。

## 构建与交付：通过

- 全部自研镜像已在目标机实际构建为 `linux/amd64`，不是使用脚本替身或只做配置解析。
- 完整交付包含公共基础设施、platform、frontend、customer-opportunity、customer-portal、contract、project、settlement、data-analysis、deployment-assets、独立安装入口和总 SHA 清单。
- 构建端与独立交付目录的 `SHA256SUMS` 均实际通过。
- 前端 HTTP、HTTPS 和 HTTPS 禁用三种入口配置均通过 Nginx 语法及缺失子系统提示回归。
- Docker Hub CDN 与官方 Go 代理在该网络存在重置或超时；最终构建通过可配置依赖源、有界重试和离线公共镜像完成，未把失败重建覆盖到已完成产物。

## 基础平台与统一前端：通过

2026-09-28 最终复核中，下列服务均为 `running/healthy` 且重启次数为 0：

- `platform-api`
- `platform-worker`
- `file-gateway`
- `keycloak`
- `temporal`
- `subsystem-provisioner`
- `frontend`

`deploy.sh verify` 返回成功；File Gateway 匿名代理请求被正确拒绝。浏览器实际打开 `http://192.168.31.153/`，登录页布局、样式和静态资源加载正常。公开 Keycloak OIDC discovery 地址可达。

## 业务子系统受控接入：五项通过

按依赖顺序完成 `prepare → 平台采用/重试 → continue → verify`，未绕过平台下发凭据：

| 子系统 | 结果 | 主要运行证据 |
| --- | --- | --- |
| 客户与商机 | 通过 | API healthy；7 类业务 Worker 运行；数据库 healthy |
| 客户自助门户 | 通过 | API healthy；补偿 Worker 运行；数据库 healthy |
| 合同管理 | 通过 | API healthy；数据库 healthy |
| 项目管理 | 通过 | API 与 SLA Worker healthy；数据库 healthy；共享 Temporal healthy |
| 结算管理 | 通过 | API 与 Worker healthy；数据库 healthy |

从验收客户端实际访问统一前端及五个子系统 `/healthz`，均返回 HTTP 200。各子系统 `deploy.sh verify <component>` 均输出数据库、API、Worker 与跨系统依赖验收通过。

## 数据分析：基础设施阻塞，未通过

- 候选目录能被平台发现和采用，运行时凭据已由受控流程下发。
- 数据分析 API、aggregation worker、alert worker 与 MySQL 能启动并通过各自健康检查。
- `data-analysis-metabase` 在 JVM 启动后长期无法完成应用初始化，Compose 最终报告容器 unhealthy。
- 同期目标机约 3.3 GiB 物理内存被耗尽，3.8 GiB swap 全部占用，SSH 与 Docker API 出现显著停顿；这与手册规定的完整部署至少 16 GiB 内存不符。
- 已执行 `deploy.sh disable data-analysis`，停止该模块并保留数据库卷、运行时配置和接入数据；基础平台与五个已通过子系统恢复可用。
- 当前状态重新回到 `WAITING_FOR_PLATFORM_ADOPTION`。只有迁移到满足至少 16 GiB 内存基线的机器后，才允许重新采用并验收；不得通过延长无限等待、删除探针或降低健康标准制造成功。

## 本轮实机发现并修复的问题

- Docker/containerd 导入后 `.Id` 语义可能是 image index digest：改为重新导出精确标签并按 config digest 校验。
- 大型归档不再默认使用容量较小的 `/tmp`，改用部署分区下的受控导入临时目录。
- Docker Socket Proxy 的 HAProxy 日志级别、健康门禁及 Agent 的最小有效权限修正。
- 首次安装没有旧不可变镜像时，失败回滚不再尝试拉取公网占位镜像。
- Compose v2/v5 的服务范围查询兼容。
- 六类子系统数据库 DSN、运行时接入键、授权目录摘要和 HTTP SSO 端口派生修正。
- Docker 29 的 `docker stop --time` 弃用参数改为 `--timeout`。
- 平台 Agent 的初始化 one-shot 改为唯一命名：失败保留供诊断，成功后精确删除，避免成功容器持续残留。

最后一项初始化 one-shot 修复已经通过本地 Go 回归，并在 Docker Desktop 上交叉构建为新的 `linux/amd64` platform 包；包内架构、config digest、sidecar 和总 SHA 均通过。目标机在最终重建期间变为 SSH/ICMP 不可达，未进行宿主机重启，因此该 v2 镜像尚未重新安装到目标机。本报告不把“新 v2 镜像实机升级”列为已通过；此前五个业务子系统的运行验收基于已部署的上一 platform 修复版本。

## 未执行与剩余风险

- 宿主机重启恢复：按约束未执行。
- 长期稳定性／压力／容量测试：未执行。
- 真实生产数据的数据库与 File Gateway 恢复：未执行；仅完成脚本级安全回归。
- 完整数据分析业务验收：受目标机内存阻塞。
- 最终 v2 platform 镜像在目标机上的升级/回滚：目标机网络不可达，尚未执行。
- HTTP 当前只适用于受控内网；公网生产使用前仍必须完成 HTTPS 证书和安全组验收。

## 资源保留与清理

- 数据分析使用保留数据的停用，不删除其数据库卷、运行配置和接入信息。
- 失败迁移容器按设计保留供原生 Docker 诊断；不得用全局 `docker system prune` 清理。
- 构建源码、构建输出和交付目录保留，便于复核摘要与重新生成最终介质。
- 未删除任何无法确认归属的数据。
