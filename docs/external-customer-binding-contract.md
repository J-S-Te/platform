# 外部客户绑定契约与对账操作

> 源码核验日期：2026-10-13。平台只维护身份与不透明客户标识的受保护绑定；无人工控制台绑定入口，不代表 CRM 邀请、门户映射或真实部署已全部验收。

## 角色、前提与端点

调用者为验签的应用机器身份，不是浏览器管理员。绑定/读取使用 `portal_mapping_provision`，禁用使用 `portal_mapping_disable`；不要与门户自身 identity_mapping scope 混用。租户/Client 从可信身份取得，平台身份须存在且未停用。

| 操作 | 路径（前缀 /api/v1/internal） |
| --- | --- |
| 绑定或恢复 | `PUT /external-users/{platform_user_id}/customer-binding` |
| 禁用 | `POST /external-users/{platform_user_id}/customer-binding/disable` |
| 对账 | `GET /external-users/{platform_user_id}/customer-binding` |

写接口 JSON 为 `{"customer_ref":"CRM-CUST-2001"}`，携带 Idempotency-Key、X-Integration-Timestamp（RFC3339，当前时刻前后 5 分钟）、X-Integration-Nonce。键与 nonce 长度上限 128，nonce 在租户域 10 分钟内不得重复。GET 只需匹配机器身份/scope，不要求写接口防重放头。

实际校验先 trim：platform_user_id 非空且不超过 128 字节，业务含义为 iam_user.id，但此层不是显式“必须 26 位 ULID”的校验；customer_ref trim 后 1..64 字节，拒绝控制字符，允许内部普通空格。不要将字符数写成字节数或声称拒绝一切空白；CRM/门户可有更严输入规则，应分别核验。

## 操作步骤与幂等限制

1. CRM 等服务核实平台身份和本系统客户资格，生成稳定幂等键及新 nonce、时间戳，提交绑定。
2. 成功后 GET 对账，检查 platform_user_id、application_code、ACTIVE；响应不回显 customer_ref、密文或摘要。
3. 同客户在同租户/应用已绑定另一身份会冲突；同身份绑定可 upsert 替换/恢复。平台事务同时处理 nonce、幂等记录、绑定和审计，不修改 CRM 客户业务数据。
4. 禁用必须指定当前匹配的 customer_ref 且绑定为 ACTIVE；新幂等键重复禁用已禁用绑定会 409，而不是无条件成功。
5. 原幂等键同规范化请求可重放响应，但每次重试必须用新 nonce 和当前时间。先检 nonce 再查幂等结果，原 nonce 的直接重发会冲突；同键异请求也冲突。幂等响应不是永远最新的业务状态，必要时再次 GET。

不存在/停用身份或不存在绑定按 404；冲突 409；校验 422。缺失保护密钥时写入失败关闭，可返回 503 INTERNAL_ERROR；数据库等错误可为 500。保留 request_id 排查，绝不把真实客户标识、Token 或密钥复制到公共工单。

## 存储、声明与恢复

迁移对“租户/身份/应用”和“租户/customer_ref_digest”建立唯一约束，并将 application_code 限制为 customer_portal；摘要输入本身含应用域。保护为 HMAC-SM3 摘要（输入含 customer、tenant、application 域分隔）与 AES-256-GCM 密文；禁用保留密文/摘要及审计，不物理删除记录。

`AUTHZ_CONTEXT_CUSTOMER_REF_ENABLED` 开启后，面向客户门户的 `/oauth2/authorization-context` 在身份/绑定活动且解密成功时追加可选 customer_ref；缺失/解密失败省略，消费者必须按无绑定失败关闭。不自动修改 Keycloak Client/ID Token Mapper，也不自动撤销 CRM 业务授权或证明门户会话即时失效。

保护密钥分别为 `IAM_CUSTOMER_REF_DIGEST_KEY`、`IAM_CUSTOMER_REF_ENCRYPTION_KEY`。格式错误会导致配置失败，缺失不降级明文。当前实现没有自动双 key 读取/轮换；双 key 迁移是未来受控方案需求，不是现成操作能力。恢复或轮换交部署/数据负责人另行审核，不能盲目换 key 后期待旧绑定正常解密。

依据：[平台核验记录](../../.ai/modules/operation-docs-platform-20261013.md)。
