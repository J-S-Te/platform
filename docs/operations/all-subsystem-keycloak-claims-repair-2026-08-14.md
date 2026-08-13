# 全部子系统 Keycloak Claims 修复记录（2026-08-14）

## 故障现象

合同管理系统可以正常进入，但从统一门户进入其他子系统时分别出现：

- 客户与商机管理：`COMMON_UNAUTHENTICATED`
- 客户自助门户：`PORTAL_OIDC_INVALID_CLAIMS`
- 项目服务内容管理：`OIDC授权声明无效`

## 根因

三个存量 Keycloak Client 只有将平台 `identity_id` 映射为 OIDC `sub` 的旧 Mapper，没有额外输出独立的 `identity_id` Claim。子系统或基础平台授权上下文严格验证该 Claim，因此在授权码兑换成功后返回 401。

此外还发现：

- 客户门户从 ID Token 解析出 `identity_id` 后，构造本地 Claims 时漏传该字段。
- 客户门户和项目管理没有统一使用 `kc_idp_hint=basic-platform`。
- 基础平台数据库保存的历史就绪状态没有触发存量 Client 的实际 Keycloak Mapper 对账。
- 项目运行配置缺少 `OIDC_SESSION_ENCRYPTION_KEY_BASE64`，导致发布清单中的新镜像无法启动。

## 生产服务器即时修复

为以下 Client 增加了过渡期独立 `identity_id` Mapper，并通过 Keycloak Admin API 回读验证：

- `customer_and_opportunity-prod-web`
- `customer_portal-prod-web`
- `project_management-prod-web`

过渡 Mapper 同时输出到 ID Token、Access Token、UserInfo 和 Introspection。名称使用 Client 专属前缀，避免当前旧版平台控制面在新镜像发布前将其误删。

项目运行配置已生成 32 字节随机会话加密密钥，文件权限保持 `0600`，密钥未记录在本文档或命令输出中。

项目系统已重新发布清单指定镜像：

```text
project_management@sha256:a60433e590eda9a8468089b2d320494b452b108bcd601d810935d5c64deb9813
```

发布前数据库备份：

```text
/opt/basic-platform/backups/project-20260813T155830Z.sql.gz
```

客户门户已基于提交 `dd80c74` 构建并发布仅替换 `portal-server` 的不可变热修复镜像，CRM 继续使用原不可变镜像：

```text
customer_and_opportunity@sha256:12ad2575978b91c504f2c4dbe61299a9ce745e475bc98f094d52787bac495bd3
```

发布前数据库备份：

```text
/opt/basic-platform/backups/customer-20260813T160717Z.sql.gz
/opt/basic-platform/backups/portal-20260813T160717Z.sql.gz
```

三个运行配置已补齐并保持 `0600` 权限：

- `runtime/customer.env`: `OIDC_IDP_HINT=basic-platform`
- `runtime/portal.env`: `PORTAL_OIDC_IDP_HINT=basic-platform`
- `runtime/project.env`: `OIDC_IDP_HINT=basic-platform`

基础平台 Worker 已发布存量 Client 对账与 Keycloak 审计路径修复镜像：

```text
platform@sha256:2089dcb5abfecb8158f74dbbec0a168747ab3202430c91933602da57df3e669e
```

同时修复了审计采集器重复拼接 `/admin/realms/{realm}` 导致的 404。发布后 Worker 日志不再出现 `collect Keycloak audit events` 错误，平台 `/readyz` 与客户门户 `/healthz` 均通过。

## 永久代码修复

### 基础平台

- Keycloak 投影协议升级到 `stable-identity-projection-v5`。
- Worker 启动时对全部已同步存量 Client 执行真实 Admin API 对账。
- 对账前撤销 Client/角色目录就绪门禁；只有 Client Mapper、角色和映射持久化全部成功后才恢复。
- 存量 Client 自动获得正式托管的独立 `identity_id` Mapper。

### 客户与商机及客户门户

- CRM 的 Broker hint 从硬编码改为 `OIDC_IDP_HINT` 配置。
- 客户门户补齐 `IdentityID: raw.IdentityID`。
- 客户门户支持 `PORTAL_OIDC_IDP_HINT` 并下发 `kc_idp_hint`。

### 项目管理

- 支持 `OIDC_IDP_HINT` 并在授权请求中下发 `kc_idp_hint`。

### 生产接入模板

客户与商机、客户门户、项目管理的生产接入清单和运行模板均默认下发 `basic-platform` Broker hint。

## 验证

- 客户与商机管理已通过浏览器完整跳转并进入客户列表。
- 项目管理在补齐 Mapper并发布新镜像后，已通过浏览器完整跳转并进入系统首页。
- 客户门户的新镜像健康检查通过，浏览器回调已从 `PORTAL_OIDC_INVALID_CLAIMS` 推进到 `PORTAL_IDENTITY_NOT_PROVISIONED`，证明 Keycloak Claim 与本地 Claims 传递均已生效。
- `PORTAL_IDENTITY_NOT_PROVISIONED` 是客户数据隔离门禁，不是 Keycloak 故障：客户门户会话必须绑定一个明确的 `customer_id`。基础平台内部账号即使拥有平台管理员权限，也不能自动绑定任意客户；需通过现有客户邀请/预配流程创建 `portal_identity_links` 后才能进入客户门户。

## 后续清理

Worker 启动对账后，三个 Client 均已确认只有一个正式 Mapper `platform-stable-identity-id`；Client 专属过渡 Mapper 已删除，未再保留重复 Claim 来源。
