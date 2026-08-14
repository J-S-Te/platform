# 外部客户绑定契约（External Customer Binding Contract）

> 版本：v1（2026-08-14，Phase 0 冻结）
> 关联方案：`sharedDocs/01_架构设计/客户门户独立化与公网暴露方案.md`
> 服务端实现：`platform/internal/platform/externalidentity`；消费方：CRM（`customer_and_opportunity`）、客户门户（`customer_portal`）

## 1. 范围

本契约定义"平台身份 ↔ CRM 客户标识"绑定的机器接口与 authorization-context 声明。平台只保存
键控 SM3 摘要与 AES-256-GCM 密文，不理解 customer_ref 的业务语义；明文只在服务端受控路径解密下发。

## 2. 通用要求（所有端点）

- 认证：应用 Bearer（client_credentials），scope 见各端点；非匹配 scope 一律 403。
- 防重放请求头（与既有 external-identity 机器接口一致）：
  - `Idempotency-Key`：`<=128` 字符，相同键只接受同一规范化请求，否则 409；
  - `X-Integration-Timestamp`：RFC3339Nano UTC，前后 5 分钟窗口，否则 409；
  - `X-Integration-Nonce`：`<=128` 字符，平台 10 分钟内唯一（tenant 域），重复即 409。
- 错误码沿用平台 envelope（`code/message/request_id`）：`COMMON_VALIDATION_ERROR`(422)、`COMMON_CONFLICT_ERROR`(409)、`COMMON_NOT_FOUND_ERROR`(404)、`INTERNAL_ERROR`(500)。

## 3. 端点

### 3.1 写入绑定（BIND）

```
PUT /api/v1/internal/external-users/{platform_user_id}/customer-binding
Scope: portal_mapping_provision
```

请求体：

```json
{ "customer_ref": "CRM-CUST-2001" }
```

- `platform_user_id`：`iam_user.id`（OIDC identity_id），26 位 ULID；
- `customer_ref`：`1..=64` 字符，前后无空白，CRM 持有的不透明客户标识。

响应 200：

```json
{ "code": "OK", "data": { "platform_user_id": "01KZ...", "application_code": "customer_portal", "status": "ACTIVE" } }
```

语义：幂等 upsert；已存在绑定更新状态为 ACTIVE 并刷新密文/摘要；身份不存在、身份 DISABLED、
同客户已绑定其他身份 → 409/404。明文 customer_ref 不回显。

### 3.2 禁用绑定（DISABLE_BIND）

```
POST /api/v1/internal/external-users/{platform_user_id}/customer-binding/disable
Scope: portal_mapping_disable
```

请求体同 3.1（需携带 `customer_ref` 以定位绑定）。成功响应 status 为 `DISABLED`。

### 3.3 对账读取

```
GET /api/v1/internal/external-users/{platform_user_id}/customer-binding
Scope: portal_mapping_provision
```

响应 200：

```json
{ "code": "OK", "data": { "platform_user_id": "01KZ...", "application_code": "customer_portal", "status": "ACTIVE" } }
```

无绑定 → 404。不返回 `customer_ref` 明文或密文。

## 4. authorization-context 扩展

`GET /oauth2/authorization-context` 响应在启用 `AUTHZ_CONTEXT_CUSTOMER_REF_ENABLED` 后，
对命中 ACTIVE 绑定的 subject 追加**可选**顶层声明（解密自平台侧密文，仅服务端路径）：

```jsonc
{
  "sub": "...", "identity_id": "...", "tenant_id": "...",
  "client_id": "customer_portal-dev-web",
  "application_code": "customer_portal",
  // ...现有字段不变...
  "customer_ref": "CRM-CUST-2001"   // 仅存在 ACTIVE 绑定且解密成功时出现
}
```

- 绑定缺失/解密失败：**省略该声明**（消费方按无绑定 fail closed），不改变其余字段；
- 不改 Keycloak、不改 ID Token、不改 claim mapper；
- 消费方（门户）校验：`customer_ref` 非空、无空白、`1..=64` 字符。

## 5. 平台存储与保护口径（决策 1：SM3）

| 字段 | 原语 | 说明 |
| --- | --- | --- |
| `customer_ref_digest` BINARY(32) | HMAC-SM3（`IAM_CUSTOMER_REF_DIGEST_KEY`） | 输入 `"customer\x00"+tenant_id+"\x00"+application_code+"\x00"+customer_ref`；查重/唯一约束 |
| `customer_ref_cipher` VARBINARY(256) | AES-256-GCM（`IAM_CUSTOMER_REF_ENCRYPTION_KEY`） | 仅 authorization-context 服务端解密下发 |

两密钥未配置时，绑定写入失败关闭（`INTERNAL_ERROR` 或服务不可用），无明文降级路径。
密钥轮换需要：双 key 读取过渡 + 按租户重算摘要的受控迁移。

## 6. 兼容性承诺

- 新增字段均为可选/追加，旧客户端不受影响；
- `customer_ref` 声明只在开关启用后出现；开关默认关闭；
- 端点路径、scope、幂等语义一经发布不得破坏性变更；新增字段须先加后删。
