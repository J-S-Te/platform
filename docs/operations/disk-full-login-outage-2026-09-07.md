# 2026-09-07 磁盘满导致登录异常：处置归档

本文为历史故障记录，数据仅代表 2026-09-07 10:48–10:52（Asia/Shanghai）的现场检查，不作为实时健康状态或自动清理指令。

## 故障与依据

- 生产主机根分区容量约 59GB，使用率 100%，可用空间为 0；inode 使用率仅 5%。
- 平台 MySQL 与 Keycloak MySQL 均记录 `Disk is full writing './binlog.…'`、`OS errno 28 - No space left on device`，等待释放空间后重试。
- Keycloak discovery 从服务器本机和外部访问均超时；多个子系统登录入口超时。首页、平台 readyz 和子系统 healthz 仍返回 200，不能据此判断认证链路可用。

## 已执行处置

用户明确授权清理空间后，核对全部容器（包括已停止容器）引用的镜像，使用不带 force 的 `docker image rm` 删除 31 个未被容器使用的旧业务镜像。保留当前运行镜像及较近的历史版本；未删除数据库卷、binlog、备份、业务文件、容器或日志，未重启服务。

删除的镜像短 ID（仅供审计，不应直接作为未来清理清单）：

```text
50668987e741 bb3093ad285b 7367a0a5e67b 83c83fa83f10
1bfdc9e9bd46 64e6b7b084fd e382067b50c7 8745b0bbacbb
68d3f809232a 92c3819989a2 e71a59eaecd5 448ca32e60c0
928a1082ea1a f47c5477cd50 8c76e8659d5a f59cd80751eb
a14e76427364 d16e8e0fdd9b 4e5caa2dda80 77be7dafb431
649ab3a9aa8f 868798d23c74 640b121baa7c 87b779fdfb7e
a5caf255e1cb 337ad419bb44 e996b010e5ae e17dd6f5db82
3ff628bdda1d 97ee8e20e693 9d3747773bb0
```

删除的是服务器本地镜像副本；能否重新拉取取决于镜像仓库是否仍保留对应 digest，本次未验证远端保留状态。

## 验证结果

- `df -h /`：使用率降至 95%，可用空间约 3.0GB。
- `docker system df`：镜像占用由 16.23GB 降至 12.17GB；共享层与文件系统统计口径不同，不能直接作为可用空间增量。
- 平台及 Keycloak 数据库进程状态检查未见磁盘等待；数据库自动恢复，无需重启。
- 平台、Keycloak、合同、CRM、Portal 的 MySQL 最后 60 秒日志检查未发现新的磁盘满错误（短观察窗口，不代表长期保障）。
- Keycloak discovery：本机和外部均 HTTP 200；平台 readyz HTTP 200。
- 项目、合同、CRM、Portal、数据分析的 `/auth/login` 均恢复 HTTP 302；启用内存 Cookie 后跟随完整跳转均最终 HTTP 200。
- 未使用业务账号提交凭据，未验证登录后的业务操作；上述结果仅证明认证入口与页面跳转恢复。

## 后续事项（未实施）

- 根分区仍为 95%，需要扩容及明确的镜像、备份、数据库日志保留策略。
- 已检查的平台 API、Keycloak、平台 MySQL 的 Docker 日志驱动为 json-file，LogConfig.Config 为空；应补齐轮转并按发布流程生效。
- 增加磁盘容量告警与认证链路探测，避免容器 healthy 掩盖数据库写入阻塞。
