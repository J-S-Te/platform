# 基础能力平台前端

当前阶段提供统一登录页，以及仅包含**系统设置**、**审计日志**的基础能力平台控制台；技术栈为 **Vue 3 + JavaScript + Vite**。

## 本地运行

```bash
npm install
npm run dev
```

访问：

- `http://localhost:5173/`：系统设置（默认首页）
- `http://localhost:5173/audit`：审计日志
- `http://localhost:5173/login.html`：统一登录页

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


## 当前页面范围

本次仅实现系统设置与审计日志两个页面。其中系统设置已包含**用户与角色**标签：用户、登录账号、角色、角色绑定和数据范围均按平台模型展示。合同、项目、报销等业务模块仍未创建页面，留待后续模块合并。

审计记录、系统设置以及用户与角色数据当前均为前端示例状态：筛选、查看详情、账号或角色状态切换和导出交互可用；持久化将在 Go 单体应用接口与 MySQL 模型接入后实现。
