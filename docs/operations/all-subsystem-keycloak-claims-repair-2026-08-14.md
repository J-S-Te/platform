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
- 客户门户的服务器 Mapper 已修复；其本地 Claims 漏传修复需要构建并发布新镜像后完成最终浏览器验收。

## 后续清理

部署包含本次基础平台修复的新镜像并完成一次 Worker 启动对账后，应确认每个 Client 均有正式 Mapper `platform-stable-identity-id`，随后删除此次创建的 Client 专属过渡 Mapper，避免长期保留重复 Claim 来源。
