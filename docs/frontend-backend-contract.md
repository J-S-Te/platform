# 前后端接口协作规范

## 1. 调用与鉴权

前端 API 基地址来自 `VITE_API_BASE_URL`，默认 `/api/v1`。控制台登录成功后由后端通过 `Set-Cookie` 下发 JWT Cookie；所有 API 调用均使用 `credentials: 'include'`。前端不能读取、持久化或手工拼接 Cookie/JWT。

所有请求由后端生成或透传 `X-Request-ID`；响应体中的 `request_id` 用于前后端联调定位。跨域开发环境需使用精确 Origin 白名单和 `Access-Control-Allow-Credentials: true`，不得使用 `*`。

## 2. 统一格式

```json
{
  "code": "OK",
  "message": "操作成功",
  "request_id": "01J...",
  "data": {}
}
```

```json
{
  "code": "AUTH_ACCOUNT_LOCKED",
  "message": "账号已锁定，请稍后再试",
  "request_id": "01J...",
  "details": { "locked_until": "2026-07-16T04:30:00Z" }
}
```

列表查询统一使用：

```text
?page=1&page_size=20&keyword=admin&filter[status]=ACTIVE&sort=-created_at
```

`page` 从 1 开始，`page_size` 默认 20、最大 100。空列表必须返回 `items: []` 和准确的 `total`，不返回 `null`。

## 3. 控制台关键接口

| 前端能力 | 方法与路径 | 后端返回重点 |
|---|---|---|
| 密码登录 | `POST /auth/login` | `data.redirect_url`、会话过期时间，及 `Set-Cookie` |
| 当前登录信息 | `GET /auth/me` | 当前用户、账号、租户、角色、权限摘要 |
| 退出登录 | `POST /auth/logout` | 撤销当前 `iam_session` 并清理 Cookie |
| 用户与账号 | `GET/POST/PATCH /users`、`GET/PATCH /accounts` | 版本号、状态、账号/任职摘要 |
| 组织任职 | `GET/POST/PATCH /org-units`、`/positions`、`/memberships` | 层级、任职类型、生效时间 |
| 角色权限 | `GET/POST/PATCH /roles`、`/role-bindings`、`/permissions` | 权限编码和数据范围 |
| 审计列表 | `GET /audit/events` | 只返回当前页面允许显示的字段 |
| 审计详情 | `GET /audit/events/{id}` | 明细、变更摘要；不返回主体/Request/Trace 字段 |
| 审计导出 | `POST /audit/export-jobs`、`GET /audit/export-jobs/{id}` | 异步任务状态和一次性下载地址 |
| 基础设置 | 配置中心命名空间接口 | 当前发布版本、配置条目、版本号 |

详细字段以 `api/openapi/platform-p0.yaml` 为唯一接口契约。

## 4. 写操作约束

- 写请求必须使用当前登录主体和租户上下文，禁止由前端传入 `tenant_id`、`created_by`、`updated_by` 覆盖服务端值。
- 更新聚合根时提交 `version`；版本冲突返回 `409` 与模块化错误码。
- 用户、角色、权限、绑定、配置发布和会话安全操作均必须写审计。
- 审计日志没有删除接口。前端的历史 mock 删除行为在接入 API 后必须移除或禁用。

## 5. P1 占位约定

通知、字典、登录安全、风险事件、审计接收状态、服务遥测与告警规则的路径已写入 OpenAPI，但默认可能返回 `501 PLATFORM_CAPABILITY_NOT_ENABLED`。前端收到该错误应展示“能力待后端启用”，不得伪造成功保存或修改本地状态。
