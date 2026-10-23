# Keycloak 配置与运维入口

> 当前源码核验：2026-10-13；不表示本目录配置已在真实服务器验收。

此目录承载 Keycloak 配置材料，实际服务由本地 `compose.local.yaml` 或生产
`deploy/production/docker-compose.yml` 装配。运维不要使用已删除的根 `compose.yaml`。

## 本地／生产步骤

1. 由持 Docker 权限的运维准备与已有数据库卷一致的密码及平台配置；实际 env 和 Secret 0600。
2. 本地按 [本地 Docker 操作](../docs/local-docker-operations.md) 启动完整依赖，不能只启用 profile 而忽略其它必需 env 文件。
3. 生产按 [离线 Runbook](../deploy/production/OFFLINE_RUNBOOK.md) 的平台发布链路启动 keycloak-db／Keycloak；
   单独复制本目录不能完成生产部署。
4. 配置公开根地址和 Realm，Issuer 必须为 `<公共根地址>/realms/<Realm>`；
   内部连接地址与公开 Issuer 分别配置，不能让浏览器访问内部 Docker 主机名。
5. 用平台受控同步／切换流程管理 Client 和运行凭据，等待后台 READY 并验证 discovery／JWKS／回调及一次真实登录。

Client 同步并非“未来待实现”：当前平台有专用同步、Keycloak 切换及平台认证回退端点，均受租户和权限控制。
回退不是直接改 `issuer_alias` 或 runtime 文件；需要服务端控制流程、凭据／投影处理与业务验证。
普通运行更新不会自动证明认证切换、角色授权或商业授权确认完成。

## 失败处理

端口 9000 可连接不等于 HTTP ready，更不等于登录可用。
缺 Client／broker 凭据、回调不匹配、迁移或后台部署失败时，查看脱敏错误码并按受控流程重试；
不修改数据库状态冒充成功，不复用其它应用凭据，不关闭认证。
默认包装入口不加载 HA 覆盖文件，多节点配置需要单独评审和验收。

备份／恢复以及专用单库脚本的锁、Docker 依赖和破坏性边界见
[Keycloak 生产运维](../docs/keycloak-production-operations.md)。
历史 Realm／事故说明只作历史依据，不代表本次已迁移或通过验收。
