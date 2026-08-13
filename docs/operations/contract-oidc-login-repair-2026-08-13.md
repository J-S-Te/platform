# 合同管理系统 OIDC 登录故障修复记录（2026-08-13）

## 故障现象

- 从子系统门户进入合同管理系统后，曾停留在 Keycloak 登录页或账户资料补全页。
- Broker 回调返回 `OIDC authorization claims are invalid`。
- 新版合同镜像发布后，合同 API 因授权目录同步返回 HTTP 500 而持续重启，健康检查超时并触发回滚。

## 根因

1. 合同 OIDC 登录代码没有读取和传递 `OIDC_IDP_HINT`，授权请求缺少 `kc_idp_hint=basic-platform`，因此 Keycloak 会先展示自己的登录页。
2. Keycloak 合同 Client 只把平台用户属性 `identity_id` 映射成 OIDC `sub`，没有输出合同服务要求的独立 `identity_id` 声明。
3. 平台授权目录中存在已禁用的历史权限 `contract_template.manage`。新版目录将其更名为 `contract.template.manage`，但两者使用相同的 `resource_id + action`。旧同步逻辑只按权限码查找，错误地尝试新增记录，触发数据库唯一键冲突。
4. 生产合同运行配置缺少 `OIDC_SESSION_ENCRYPTION_KEY_BASE64`，新版合同服务无法安全持久化 OIDC 会话。

## 服务器修复

- 在 `/opt/basic-platform/runtime/contract.env` 中生成并保存 32 字节随机会话加密密钥，文件权限保持为 `0600`。密钥未写入本文档或日志。
- 将历史权限记录 `contract_template.manage` 原位迁移为 `contract.template.manage`，保留原记录 ID、资源和动作关系，并恢复为 `ACTIVE`。
- 为 Keycloak Realm `basic-platform` 的 Client `contract_management-prod-web` 新增过渡 Mapper `contract-management-stable-identity-id`（使用非 `platform-` 前缀，避免旧版控制面在新平台镜像发布前把它当作过期配置删除）：
  - `user.attribute = identity_id`
  - `claim.name = identity_id`
  - ID Token、Access Token、UserInfo、Introspection 均启用
- 重新发布合同镜像 `sha256:d83b83baa240629edee3926bd868393a7c21bee8cd2a220994dd58a711fa0909`。
- 发布前数据库备份：`/opt/basic-platform/backups/contract-20260813T143500Z.sql.gz`。

## 本地代码修复

### 合同管理系统

- 读取 `OIDC_IDP_HINT`，在授权请求中携带 `kc_idp_hint`。
- UserInfo 的 subject 校验改为使用 OIDC 原生 `sub`，不再错误使用平台 `identity_id`。
- 新增配置和授权 URL 回归测试。

### 基础平台

- Keycloak Client 自动接入逻辑新增独立 `identity_id` Mapper，后续新建或重新同步子系统时自动下发。
- 授权目录 Upsert 在权限码未命中时，按 `resource_id + action` 识别历史权限并原位更新，兼容权限码更名。
- 新增 Mapper 和权限码迁移回归测试。

## 验证结果

- 合同 API 发布健康检查通过，容器不再重启。
- Keycloak 合同 Client 已存在独立 `identity_id` Mapper。
- 浏览器实测链路成功：

  `子系统门户 → Keycloak/Broker → /contract_management/auth/callback → 合同管理工作台`

- 合同工作台显示“已安全登录”，用户为“平台管理员”，角色为“超级管理员”。

## 后续部署注意事项

- 新环境必须为合同服务提供 `OIDC_SESSION_ENCRYPTION_KEY_BASE64`，不得在不同发布间随机更换，否则已有会话会失效。
- Keycloak 配置应由基础平台的“接入/同步子系统”流程托管，不要手工创建同名 Mapper。
- 发布包含本次平台修复的新镜像并重新同步 Client 后，平台会创建正式托管 Mapper `platform-stable-identity-id`；届时可删除过渡 Mapper `contract-management-stable-identity-id`。
- 部署包含合同端 `OIDC_IDP_HINT` 修复的新镜像后，应使用无痕窗口再次验证，确认全程不出现 Keycloak 原生登录或资料补全页面。
