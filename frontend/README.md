# 基础能力平台前端

当前阶段提供统一登录页，技术栈为 **Vue 3 + JavaScript + Vite**。

## 本地运行

```bash
npm install
npm run dev
```

访问：

- `http://localhost:5173/`
- `http://localhost:5173/login.html`

## 构建

```bash
npm run build
```

构建产物位于 `dist/`，可由 Go 单体应用作为静态文件提供。

## 登录接口约定

页面默认调用：

```text
POST /api/v1/auth/login
Content-Type: application/json

{
  "account": "用户名、手机号或邮箱",
  "password": "用户输入的密码",
  "login_type": "password"
}
```

推荐后端通过 `HttpOnly + Secure + SameSite` Cookie 下发会话凭证。前端不会把 access token 或 refresh token 写入 `localStorage` / `sessionStorage`，仅在用户勾选“记住账号”后保存账号名。

可复制 `.env.example` 为 `.env.local` 修改 API 地址和登录成功跳转地址。

原始静态页面已备份到 `prototype/login.reference.html`。
