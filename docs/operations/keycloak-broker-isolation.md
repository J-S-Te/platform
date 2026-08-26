# Keycloak Broker 隔离

平台内部员工和客户门户不能共用同一个 Basic Platform Broker 客户端。OAuth 客户端对应的应用和环境决定了令牌中的授权上下文；共用 `keycloak-broker` 会使其中一类登录拿到另一类应用的 claims，最终表现为 `OIDC_INVALID_CLAIMS` 或身份提供商认证失败。

## 固定关系

| 场景 | Keycloak IdP alias | 平台 OAuth client | 平台应用/环境 |
| --- | --- | --- | --- |
| 销售、平台和内部子系统 | `basic-platform` | `keycloak-broker` | `platform / dev|prod` |
| 客户自助门户 | `basic-platform-customer` | `keycloak-customer-portal-broker` | `customer_portal / dev|prod` |

Worker 启动时会确保两个 OAuth 客户端存在并校准两个 IdP。客户门户的投影用户也按 `customer_portal` 应用自动挂到 `basic-platform-customer`，其他应用继续挂到 `basic-platform`。

## 配置

客户门户使用 `PORTAL_OIDC_IDP_HINT=basic-platform-customer`。内部子系统继续使用 `OIDC_IDP_HINT=basic-platform`。

可选地将 Worker 恢复出的密钥放入：

```text
KEYCLOAK_PLATFORM_CLIENT_ID
KEYCLOAK_PLATFORM_CLIENT_SECRET
KEYCLOAK_CUSTOMER_PORTAL_CLIENT_ID
KEYCLOAK_CUSTOMER_PORTAL_CLIENT_SECRET
```

留空时 Worker 从平台 OAuth 客户端目录恢复；不会把客户门户密钥写入浏览器或前端构建产物。

## 发布注意事项

先发布平台 Worker/API，使两个 Broker 和客户门户投影链路生效，再重启客户门户。不要把客户门户的 IdP hint 改回 `basic-platform`，也不要通过数据库直接修改 OAuth 客户端归属来临时修复登录。
