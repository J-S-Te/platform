# 本地认证开发与联调

## 已实现范围

当前认证模块提供以下 API：

- `POST /api/v1/auth/login`：仅接受 `login_type=password`，校验本地账号的 Argon2id 密码凭据，创建 `iam_session` 并下发 HttpOnly JWT Cookie。
- `POST /api/v1/auth/token/refresh`：在有效 Cookie、会话、账号、用户和租户均有效时续期会话并替换 Cookie。
- `POST /api/v1/auth/logout`：撤销当前会话并清理 Cookie。
- `GET /api/v1/auth/me`：返回当前租户、用户、账号，以及 `platform` 应用下生效的用户角色和 `ALLOW` 权限摘要。

所有浏览器 API 调用必须携带 `credentials: 'include'`，JWT 不会出现在响应体或前端存储中。

## 准备本地密钥

`backend/.env` 使用文件路径引用 JWT 密钥，不保存私钥内容。首次运行可从项目根目录执行：

```bash
make -C backend generate-dev-jwt-keys
```

该命令会生成：

- `backend/data/keys/jwt-ed25519-private.pem`：PKCS#8 Ed25519 私钥，权限为 `0600`。
- `backend/data/keys/jwt-ed25519-public.pem`：PKIX Ed25519 公钥。

`backend/.env` 与 `backend/.env.example` 中的 `AUTH_JWT_PRIVATE_KEY_PATH`、`AUTH_JWT_PUBLIC_KEY_PATH` 已指向上述开发路径。路径相对于后端进程的工作目录 `backend/` 解析。生产环境必须由部署系统通过环境变量或受控挂载提供不同的密钥文件。

## 联调前置条件

1. 配置 MySQL 后，从项目根目录执行 `make -C backend migrate`。
2. 生成或配置 JWT 密钥。
3. 创建一个处于 `ACTIVE` 状态的租户、用户、`LOCAL` 账号和 `argon2id` 密码凭据。
4. 为需要在 `/auth/me` 中展示权限摘要的用户，创建 `subject_type=USER`、`status=ACTIVE` 的 `authz_role_binding`。

迁移初始数据**不会**创建固定密码或默认人类账号。虽然 IAM 用户、组织和账号状态管理已实现，但当前 OpenAPI 尚未定义账号创建、密码初始化或重置接口，因此首个管理员的受控初始化能力仍待补充契约后实现。

## 密码凭据存储约定

`iam_password_credential` 的 `password_hash` 存储原始 Argon2id 派生字节；`hash_algorithm` 固定为 `argon2id`；`algorithm_params` 存储如下 JSON：

```json
{
  "version": 19,
  "memory_kib": 65536,
  "iterations": 3,
  "parallelism": 2,
  "salt_base64": "...",
  "key_length": 32
}
```

后续 IAM 密码设置用例必须使用此约定，并使用 `internal/shared/security.HashPassword` 生成摘要和元数据；不得把明文密码写入数据库、日志、审计摘要或浏览器存储。

## 当前边界

登录限制、MFA、OIDC/OAuth、服务账号认证及审计事件接收/查询仍未实现。认证模块不把这些能力伪装为已经可用。
