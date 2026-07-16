# IAM 用户、账号与组织管理本地开发说明

## 已实现接口

所有接口位于 `/api/v1`，均需要已登录的 `bp_session` Cookie：

- 用户：`GET/POST /users`、`GET/PATCH /users/{user_id}`
- 账号：`GET /accounts`、`PATCH /accounts/{account_id}`
- 组织：`GET/POST /org-units`
- 岗位：`GET/POST /positions`
- 任职：`GET/POST /memberships`、`PATCH /memberships/{membership_id}`

列表响应统一为 `data.items`、`data.page`、`data.page_size`、`data.total`。用户、账号和任职的分页参数为 `page`、`page_size`、`keyword`、`filter[status]`；组织和岗位接口依据当前 OpenAPI 契约使用固定的第 1 页、每页 100 条，并仍返回同一分页响应结构。

## 手机号保护

`POST /users` 和 `PATCH /users/{user_id}` 中的 `mobile` 是可选敏感字段。服务端仅保存 AES-256-GCM 密文以及用于精确检索的 HMAC-SHA-256 摘要，响应中只返回 `mobile_masked`。

若要提交 `mobile`，在 `backend/.env` 中配置一个 Base64 编码的 32 字节密钥：

```dotenv
IAM_MOBILE_ENCRYPTION_KEY=<base64-encoded-32-byte-key>
```

本地可使用下列命令生成后粘贴到 `backend/.env`；密钥不能提交到 Git：

```bash
openssl rand -base64 32
```

未配置该变量时，服务可以正常启动和管理不含手机号的用户；携带 `mobile` 的写请求会失败，避免敏感信息以明文落库。

## 边界与后续工作

- 当前公开接口只能创建 `iam_user`，不能创建本地登录账号或初始化密码，因为现有 OpenAPI 没有账号创建、密码初始化或重置接口。首个可登录管理员仍需要受控初始化流程，建议在后续补充接口契约后实现。
- `PATCH /accounts/{account_id}` 只允许在 `ACTIVE` 与 `DISABLED` 之间切换，`LOCKED` 由密码登录失败策略维护。
- `Membership` 写入时要求用户、组织和岗位均存在，且岗位属于所选组织；同一用户同一时刻只能拥有一个有效的主任职。
- 当前仅实施认证校验。权限点（例如 `platform:user:create`）的强制决策属于后续 RBAC 优先级，当前不会声称已完成授权拦截。
- 用户、账号、组织和任职写操作的审计事件落库属于后续审计优先级，本阶段尚未写入 `audit_event`。
