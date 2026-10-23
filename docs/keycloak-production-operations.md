# Keycloak 生产运维说明

> 源码核验日期：2026-10-13；本次只读源码，没有登录服务器、执行备份／恢复或动态验收。
> 生产编排与参数以当前 `deploy/production/docker-compose.yml` 及包装脚本为准。

## 角色、装配和前置条件

生产运维维护镜像、数据库、公开入口、TLS、时间和备份；平台应用管理员维护租户内的 Keycloak Client／认证接入；
业务人员验证真实登录与授权。管理员 Secret、service-account Secret 和数据库密码仅通过受控配置交付。

生产入口统一为 `deploy/production/bin/deploy.sh`；不要混用已删除的根 `compose.yaml` 或无完整 env 参数的裸 Compose。
Keycloak 使用独立 `keycloak-db`；真实数据库密码须匹配保留卷，缺失时恢复原 Secret，不能随机生成后连接旧库。
OIDC Issuer 形如 `<Keycloak 公共根地址>/realms/<KEYCLOAK_REALM>`，公网地址与容器内部地址分开；
不得把内部 Docker DNS 改写成浏览器信任的公网 Issuer。

当前 Compose 的 Keycloak 版本引用与交付介质须单独审核，不能声称所有第三方镜像均已按 digest 锁定。
`compose.ha.keycloak.yaml` 存在，但默认生产包装入口不加载它；文件存在不代表已交付 HA 或完成多节点验收。

## 运行检查与变更

1. 核对批准的部署根目录、当前平台／Agent 版本、配置权限和升级门禁。
2. 用包装入口 `bin/deploy.sh verify` 检查平台和 Keycloak discovery；核对实际容器健康与公网地址。
3. 从客户端核对 discovery 的 issuer、JWKS、Realm、Client redirect URI，并执行一次授权测试账号登录。
4. 变更 Client／认证源使用平台受控管理流程，不以直接改 runtime Issuer 或数据库行代替认证切换。

Compose 的 9000 端口 TCP 探测只说明端口可连接，不能替代 Keycloak 的 HTTP `/health/ready`，
更不能证明真实登录或平台授权可用。不要将管理／metrics 端口公开到互联网。

平台已具有 Keycloak Client 同步、切换与回退端点；切换会校验 HTTPS／回调、协调 broker 和 Client、下发凭据并运行后台部署。
页面受理之后仍需等待 READY／认证投影完成并验证实际登录。普通运行时更新、切换认证源和授权状态确认不是同一动作。
失败检查状态错误码、Secret 交付、broker／Client、回调和依赖；不得手改 READY、复用其它应用 Secret 或关闭认证检查。

## 完整备份与优先恢复路径

完整灾备使用 [BACKUP_RECOVERY](../deploy/production/BACKUP_RECOVERY.md) 的 `backup-all.sh`：
保留 Keycloak 数据库、配套平台状态、运行配置、签名材料及版本指针。
网关停写不代表其它数据库停写；跨库一致恢复点须额外维护协调。
`--encryption-key-file` 仅加密恢复状态归档，SQL 和网关文件归档仍需加密介质及受限权限保护。

Keycloak 全批次 SQL 可通过通用 `restore-mysql.sh --service keycloak-db` 在统一部署锁和全部运行写入者检查下恢复。
真正恢复先获审批、冻结认证变更、停止全部节点／相关写入者，并在隔离环境演练。SQL 导入没有自动反向恢复；
恢复完成后人工启动获批版本，再校验 Realm／Client／JWKS／登录与平台业务。

## 专用单库备份脚本的范围

在真实部署目录中：

```bash
sudo ./bin/backup-keycloak-mysql.sh
sudo ./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-<UTC时间>.sql.gz --verify-only
```

备份在容器内读取密码，输出 gzip、SHA256 及监控时间；独立备份锁遇到并发会跳过且返回零，必须检查是否确有新批次。
它只备份 MYSQL_DATABASE，gzip／压缩文件非空不代表已经完成数据库恢复演练。
旁路 SHA 文件使用原路径，迁移备份位置后需人工核对其引用，不把路径失配当作可忽略的校验错误。
脚本同时写入 `<备份文件>.binlog-position` 和两份 SHA-256（SQL 与位点）；binlog 开启但取不到位点时该次备份失败，binlog 关闭时位点文件记录 `LOG_BIN=0`。

专用恢复只接受真实部署目录的 `backups/keycloak/keycloak-*.sql.gz`；
`--verify-only` 仍校验 Compose、数据库健康并需要 Docker，不是离线恢复机的无 Docker 校验器。
缺少 SHA 时旧脚本会告警；正式恢复应先补齐可信摘要证据，不能因此降低介质要求。
获批准的破坏性导入：

```bash
sudo ./bin/restore-keycloak-mysql.sh \
  --backup backups/keycloak/keycloak-<UTC时间>.sql.gz \
  --confirm RESTORE_KEYCLOAK_DATABASE
```

该入口清空并重建单库，仅检查同一 Compose 的 Keycloak；没有通用恢复的统一部署锁／全容器写入者检查，
不会替你停止或重启节点。必须人工停止所有认证节点并阻止并行发布；多节点或完整批次优先采用通用恢复协议。
导入失败可能留下部分数据，保留维护状态和原备份，不自动启动服务。
成功至少满足数据库可读、HTTP ready、公开 discovery／JWKS、Client 回调、实际测试账号登录和权限恢复，并记录 RTO／RPO。
