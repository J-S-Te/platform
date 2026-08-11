# Keycloak 生产运维 Runbook

> 适用范围：`deploy/production/compose.yaml` 中的 Keycloak 26.x 与独立 `keycloak-db`。
> 本文不要求 HTTPS。公开协议由网关策略和 `KEYCLOAK_PUBLIC_URL` 决定；容器可继续通过 HTTP
> 接收本机或网关流量。

## 1. 上线前基线

Keycloak 数据库与业务 MySQL 分离：只能使用 `keycloak-mysql-data` 卷和 `keycloak-db` 服务，不得把
Realm 数据写入 `platform-mysql`。创建宿主机备份目录并限制访问：

```bash
cd /opt/basic-platform
install -d -m 700 backups/keycloak
chmod 600 .env .release.env
docker compose --env-file .env --env-file .release.env -f compose.yaml config -q
docker compose --env-file .env --env-file .release.env -f compose.yaml up -d keycloak-db keycloak
docker compose --env-file .env --env-file .release.env -f compose.yaml ps keycloak-db keycloak
```

`KEYCLOAK_PUBLIC_URL` 必须等于浏览器实际访问的 issuer 基址（不含 realm）。以下两种都受支持：

| 入口策略 | 关键配置 | 适用边界 |
| --- | --- | --- |
| 本机/可信内网 HTTP | `KEYCLOAK_PUBLIC_URL=http://sso.internal:18090`、`KEYCLOAK_HTTP_ENABLED=true` | 不直接向不可信网络开放 |
| 网关终止 TLS | `KEYCLOAK_PUBLIC_URL=https://sso.example.com`、`KEYCLOAK_HTTP_ENABLED=true`、`KEYCLOAK_PROXY_HEADERS=xforwarded` | 网关传递 `X-Forwarded-Proto/Host/For`；容器内 HTTP 正常 |

`KEYCLOAK_HOSTNAME_STRICT=true` 是已有完整公开 URL 时的推荐生产默认；迁移旧网关、需要接受多个
Host 时可以暂设为 `false`，完成入口收敛后再开启。不要把管理端口 9000、MySQL 3306 或容器 8080
发布到公网；Compose 只绑定 Keycloak 的业务端口到 `127.0.0.1`。

现有 Compose 已启用的可选增强安全默认是：独立 MySQL 卷、Keycloak 专属逻辑备份挂载、`jdbc-ping`
缓存发现、管理端口不发布、health/metrics 启用和 `restart: unless-stopped`。按容量与组织策略进一步
增强时，应为 Keycloak/DB 设置 CPU、内存、磁盘配额与监控阈值；通过 Secret Manager 的文件挂载替换
明文环境文件；在网关/主机防火墙实现只允许入口到 8080、节点间集群端口互通的最小网络规则；并将
`quay.io/keycloak/keycloak:26.2` 在发布流程锁定为经验证的不可变 digest。后四项需要结合部署平台
实施，不能在单机 Compose 中伪造为已生效的安全保障。

## 2. 凭据来源、职责与轮换

所有 Secret 只可来自 Secret Manager、CI/CD 注入或权限 `0600` 的 `.env`；不得提交、打印到日志、
放入镜像标签或命令行。每项使用独立随机值，不复用浏览器 Client、Broker Client、数据库和管理员凭据。

| 凭据 | 用途 | 当前兼容性与轮换 |
| --- | --- | --- |
| `KEYCLOAK_DB_PASSWORD` | Keycloak 到独立 MySQL 的应用账号 | 在 MySQL 中新增/切换密码、重启 Keycloak；保留短暂回滚窗口 |
| `KEYCLOAK_DB_ROOT_PASSWORD` | 仅数据库初始化、备份/恢复管理 | 日常不注入应用；变更后更新受控备份作业 |
| `KEYCLOAK_ADMIN_USERNAME/PASSWORD` | 现有平台镜像调用 Admin API 的兼容凭据 | 当前必需；先在 Keycloak 创建并验证替代管理员，再更新 Secret、滚动重启平台 API，最后撤销旧密码 |
| `KEYCLOAK_BOOTSTRAP_ADMIN_SERVICE_CLIENT_ID/SECRET` | **只在 master realm 首次创建时**建立临时 bootstrap 管理服务账号 | 两项同时设置才启用；对已有数据库无效。完成最小权限常设服务账号/恢复路径验证后立即轮换或删除临时账号 |
| `KEYCLOAK_PLATFORM_CLIENT_ID/SECRET` | 平台作为 Broker Client 的凭据 | 不得用作 Admin API 或 bootstrap service account；按 Client secret 轮换流程变更 |

重要限制：本仓库当前平台二进制使用管理员用户名/密码获取 Keycloak Admin API 权限，尚未读取
`KEYCLOAK_BOOTSTRAP_ADMIN_SERVICE_*` 来执行 client-credentials。因此服务账号是 Keycloak 的可选
bootstrap/运维配置，不能删除 `KEYCLOAK_ADMIN_*` 来替代平台管理。要切换平台到 service-account
管理，必须先随应用版本实现和验收 client-credentials，再在非生产环境演练迁移。

每次轮换应记录操作者、Secret 版本、影响 Client/账号、开始/完成时间和回滚 Secret 版本。轮换后执行
第 5 节的 discovery、token 和管理同步检查；失败时只回退凭据引用，不回滚数据库或 Realm 变更。

## 3. 备份与恢复

至少每日一次全量逻辑备份、保留满足 RPO 的周期，并把加密副本复制到不同故障域。备份必须包括：

- `keycloak` 数据库逻辑备份；
- `.env`、`.release.env` 与运行时 Client Secret 的受控加密副本；
- Nginx/负载均衡入口配置、Keycloak 镜像 digest 和 Realm/Client 变更审计记录。

不要依赖 Docker 卷快照作为唯一备份。生产包提供了可直接执行的
[`backup-keycloak-mysql.sh`](../deploy/production/bin/backup-keycloak-mysql.sh)：它在数据库容器内读取
凭据、执行一致性逻辑备份、验证 gzip 和非空结果、写入 SHA-256，并使用 `flock` 防止重叠执行。
脚本不会停止服务，默认**不会删除任何历史备份**，也不会把密码带到宿主机的 cron 环境或命令行。

```bash
cd /opt/basic-platform
install -d -m 700 backups/keycloak monitoring/textfile
chmod 750 bin/backup-keycloak-mysql.sh
bin/backup-keycloak-mysql.sh
```

默认备份位于 `backups/keycloak/`，指标文件位于 `monitoring/textfile/keycloak_backup.prom`。若要执行
经过审核的保留策略，才显式设置 `KEYCLOAK_BACKUP_RETENTION_DAYS`（正整数）；例如 `14` 只清理超过
14 天的已完成 `.sql.gz` 及其校验文件。定时任务以部署用户安装，示例为每天 02:17 UTC：

```cron
17 2 * * * cd /opt/basic-platform && /usr/bin/flock -n /opt/basic-platform/backups/keycloak/.cron.lock ./bin/backup-keycloak-mysql.sh >> /var/log/basic-platform/keycloak-backup.log 2>&1
```

如使用 systemd，创建等价的 `Type=oneshot` service 和 `OnCalendar=*-*-* 02:17:00 UTC` timer，service 的
`WorkingDirectory=/opt/basic-platform`、`User=deploy`，执行同一脚本。无论 cron 或 timer，都应在备份后复制
加密副本至不同故障域；只看到任务退出成功不足以证明可恢复。

恢复只能在隔离演练环境先做。冻结写入并记录目标 RTO/RPO，停止所有 Keycloak 节点后：创建全新独立
MySQL 数据卷/实例，导入已验证 SQL，恢复与该备份时间点匹配的 Secret 和入口配置，启动相同 Keycloak
镜像 digest，再检查 issuer、Realm、Client、JWKS 和一次真实登录。不要把 SQL 导入正在服务的生产
`keycloak-db`，也不要只恢复数据库而遗漏 Client Secret 或公开 issuer。

## 4. 健康检查、监控与告警

Compose 使用 Keycloak 管理端口 9000 的 TCP 健康检查；健康和指标功能已启用。该端口不对宿主机发布。
监控 Agent 应从 Docker 网络或受限本机路径抓取 `/health/ready`、`/health/live` 和 `/metrics`，不得由
公网网关转发这些端点。数据库就绪检查依赖 metrics，故保持 `KEYCLOAK_METRICS_ENABLED=true`。

仓库提供默认关闭的 Prometheus 示例：

```bash
cd /opt/basic-platform
install -d -m 750 monitoring/textfile
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml -f compose.observability.yaml up -d prometheus keycloak-backup-metrics
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml -f compose.observability.yaml config -q
```

`compose.observability.yaml` 不发布 Prometheus、exporter、9000 或 MySQL 端口；需要查看 UI 时，由受控
运维网络另行提供反向代理或临时 SSH 隧道。`monitoring/prometheus/` 含 Keycloak `/metrics` 抓取和三个最低
规则：指标不可达、已验证备份过期、备份成功指标从未出现。告警路由（Alertmanager、PagerDuty 等）由运行环境
补充，示例不伪造外部通知已配置。

最小监控项及建议告警：

| 信号 | 告警条件 | 首先处理 |
| --- | --- | --- |
| `keycloak` health/ready | 连续 3 次失败或容器重启 | 看 `keycloak-db`、Keycloak 日志、磁盘与 Secret 版本 |
| `keycloak-db` health、连接/磁盘 | 不健康、磁盘余量 < 20%、备份失败 | 停止发布；扩容/恢复备份前避免写入 |
| `/metrics` 请求、认证错误率/延迟 | 5 分钟错误率或 p95 高于业务基线 | 区分网关、数据库池、外部 IdP 与暴力尝试 |
| Keycloak 事件/管理员事件 | 异常登录、管理员权限/Client/redirect URI 变更 | 立即审计事件、撤销可疑会话/Secret 并保全日志 |
| 备份新鲜度与恢复验证 | 超过 RPO 未完成，或最近演练失败 | 按备份失败事件处理，不能静默忽略 |

生产变更和告警应关联发布版本、Keycloak 镜像 digest、Secret 版本（不含 Secret 值）及请求/事件 ID。对
认证连续失败采用网关限流、MFA/风险策略和告警，不在 Keycloak 容器前暴露管理端点。

## 5. 日常验证与故障切换

每次发布、入口或凭据变更后执行：

```bash
cd /opt/basic-platform
docker compose --env-file .env --env-file .release.env -f compose.yaml ps keycloak-db keycloak
docker compose --env-file .env --env-file .release.env -f compose.yaml logs --tail 100 keycloak
curl -fsS "${KEYCLOAK_PUBLIC_URL}/realms/${KEYCLOAK_REALM}/.well-known/openid-configuration" >/dev/null
```

最后一条在已导出相同非敏感变量的受控 shell 中运行；也可使用实际 URL。还要以测试账号完成一次
Authorization Code + PKCE 登录、验证 issuer/JWKS、退出，并从平台页面执行一次无副作用的管理同步/健康检查。
不要使用管理员账号作普通业务登录。

Keycloak 不可用时，当前策略只允许已建立的平台本地会话继续使用；新的 Keycloak Broker 登录、Realm
Client 同步和身份切换必须失败关闭。值班人员先声明认证降级、保护错误日志和指标，再按数据库、网关、
Secret、Keycloak 容器的顺序定位；禁止为“恢复登录”临时开放公网 HTTP、关闭 issuer 校验或直接改库。

## 6. HA 与灾备演练

当前 Compose 定义是一节点 Keycloak，具有重启恢复能力但不具备实例级高可用。HA 覆盖层
`deploy/production/compose.ha.keycloak.yaml` **默认不会加载**；它会禁用本地 `keycloak-db`，移除宿主机
端口映射，并将 Keycloak 扩为至少两个副本。启用前必须已经具备：外部共享/高可用 MySQL、可到达每个副本的
外部负载均衡（含会话策略和健康检查）、以及节点间集群通信网络。仅执行 `--scale` 或复用单机 MySQL 不构成 HA。

```bash
cd /opt/basic-platform
# 这三个值只从受控 Secret/运行环境提供，不写入仓库。
export KEYCLOAK_HA_DB_URL='jdbc:mysql://keycloak-mysql-ha.example.internal:3306/keycloak?useSSL=true&serverTimezone=UTC'
export KEYCLOAK_HA_DB_USER='keycloak'
read -rsp 'External Keycloak DB password: ' KEYCLOAK_HA_DB_PASSWORD; export KEYCLOAK_HA_DB_PASSWORD; echo
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml -f compose.ha.keycloak.yaml --profile keycloak-ha config -q
docker compose --env-file .env --env-file .release.env \
  -f compose.yaml -f compose.ha.keycloak.yaml --profile keycloak-ha up -d keycloak
unset KEYCLOAK_HA_DB_PASSWORD
```

负载均衡器需在受控网络上代理副本的 8080，并从同一受控网络检查 9000 的 `/health/ready`；不要向公网转发
管理端口。`KEYCLOAK_PUBLIC_URL` 可以是 `http://`（仅本机/可信内网）或 `https://`（通常由 LB 终止 TLS），
HA 覆盖层不强制 HTTPS。先在预生产验证节点加入/退出、会话行为、负载均衡粘性和网络分区告警，再扩大规模。
数据库本身也必须具备与 RPO 相符的复制/故障转移。

每季度至少一次、每次重大版本/Realm 变更后额外一次灾备演练：

1. 选取加密备份和对应 Secret/入口配置，记录演练开始时刻及目标 RTO/RPO。
2. 在隔离网络恢复独立数据库和相同镜像，不连接生产 IdP、邮件或业务回调。
3. 执行第 5 节验证；检查 Realm、Client、redirect URI、service account、issuer/JWKS 与测试登录。
4. 模拟一节点故障（HA 环境）和数据库不可用，确认告警、值班升级与既有会话降级策略。
5. 删除演练副本或按数据保留规则销毁，归档结果、实际 RTO/RPO、发现的问题和改进负责人。

演练失败本身就是未满足恢复目标的告警：在修复并重新演练前，不得把“已备份”视为“可恢复”。
