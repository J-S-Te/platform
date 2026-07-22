# Basic Platform（基础能力平台）

Basic Platform 是一个采用 **Go + Gin + GORM + MySQL** 和 **Vue 3 + JavaScript + Vite** 构建的单体基础能力平台，用于统一承载身份、组织、权限、安全、审计、配置、日志及系统接入等公共能力，并为合同管理、项目管理、报销管理等后续业务系统提供复用基础。

> 本项目当前不采用微服务、不使用 Redis。登录会话及业务数据持久化到 MySQL；文件存储使用本地文件夹；通知能力仅包含站内信，不提供邮件和短信发送。

## 当前能力范围

当前代码已建立或正在持续完善以下能力：

- 身份、用户、登录账号、组织、岗位与任职管理
- RBAC 角色、权限、角色绑定与授权检查
- 本地账号密码认证、MySQL 会话、登录安全策略与风险控制
- TOTP MFA 登录验证及高风险操作二次验证
- OAuth 2.0 / OpenID Connect 协议与 OAuth 客户端管理
- 外部身份提供商、第三方身份绑定及钉钉扫码登录
- 接入应用、运行环境、Scope 与跨应用统一登录目标
- 审计事件上报、查询、导出、归档及失败运营基础
- 平台设置、通知设置、业务字典和站内信基础能力
- 运行日志、Trace、Metric 与告警规则的可观测性基础
- 本地文件元数据、上传下载和异步任务 Worker 基础

接口是否已经可用，应以实际 Gin 路由、HTTP Handler 和接口文档为准，不能仅根据模型或函数名称判断。

## 技术栈

| 层级 | 技术 |
| --- | --- |
| 后端语言 | Go 1.26.4（以 `backend/go.mod` 为准） |
| Web 框架 | Gin 1.10 |
| ORM | GORM 1.30 |
| 数据库 | MySQL |
| 前端 | Vue 3 + JavaScript |
| 前端开发工具 | Vite 7 |
| 会话存储 | MySQL |
| 文件存储 | 本地文件系统 |
| 异步任务 | MySQL 任务表 + Go Worker |
| 可观测性 | 结构化日志，可选 OpenTelemetry |

## 项目结构

```text
Basic-Platform/
├── .env.example                  # 全项目环境变量模板
├── api/                          # OpenAPI、接口、JWT 和数据模型文档
├── backend/
│   ├── cmd/
│   │   ├── api/                  # HTTP API 启动入口
│   │   ├── migrate/              # 数据库迁移入口
│   │   └── worker/               # 异步任务 Worker 入口
│   ├── internal/                 # 后端领域、应用、基础设施和传输层代码
│   ├── migrations/               # 显式、版本化 MySQL 迁移
│   ├── go.mod
│   └── Makefile
├── data/                         # 本地密钥、上传文件和运行日志目录
├── docs/                         # 中文架构、开发和模块说明文档
├── frontend/
│   ├── src/                      # Vue 前端源代码
│   ├── prototype/                # 原型参考文件
│   ├── index.html
│   ├── login.html
│   └── package.json
└── requirements_document/        # 原始需求和拆分后的需求文档
```

## 本地开发

### 1. 环境要求

请先安装：

- Go（版本以 `backend/go.mod` 为准）
- MySQL
- Node.js 和 npm
- OpenSSL（用于生成本地 Ed25519 JWT 密钥）

### 2. 准备 MySQL

创建一个供本项目使用的数据库，例如：

```sql
CREATE DATABASE basic_platform
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;
```

数据库账号应遵循最小权限原则，并至少拥有当前数据库的建表、变更和读写权限。

### 3. 配置根目录环境变量

后端 API、数据库迁移和 Worker 统一读取项目根目录的 `.env`：

```bash
cp .env.example .env
```

至少需要检查并填写：

```dotenv
MYSQL_HOST=127.0.0.1
MYSQL_PORT=3306
MYSQL_DATABASE=basic_platform
MYSQL_USERNAME=basic_platform
MYSQL_PASSWORD=你的数据库密码
```

涉及身份安全的加密密钥、初始化令牌和外部身份提供商配置，也必须按 `.env.example` 的说明设置。`.env` 包含敏感信息，不得提交到版本库。

### 4. 生成本地 JWT 密钥

在项目根目录执行：

```bash
make -C backend generate-dev-jwt-keys
```

密钥默认生成到 `data/keys/`。命令检测到目标目录已存在时会拒绝覆盖，避免误删已有密钥。

### 5. 执行数据库迁移

```bash
make -C backend migrate
```

项目使用显式 SQL 迁移维护数据库结构，不使用 GORM `AutoMigrate` 替代版本化迁移。

### 6. 启动后端 API

```bash
make -C backend run-api
```

默认监听地址由根目录 `.env` 中的 `APP_HTTP_ADDR` 控制，示例值为 `:8080`。

### 7. 启动异步任务 Worker

需要处理审计导出、文件清理或其他异步任务时，另开一个终端执行：

```bash
make -C backend run-worker
```

Worker 使用 MySQL 任务表协作，不依赖 Redis 或外部消息队列。

### 8. 启动前端开发服务

```bash
cd frontend
npm install
npm run dev
```

如需修改 API 地址，可复制前端环境变量模板：

```bash
cp .env.example .env.local
```

前端开发直接使用源代码运行。本仓库不保留 `node_modules/` 和 `dist/` 等依赖或构建产物。

## 默认访问地址

| 地址 | 用途 |
| --- | --- |
| `http://localhost:5173/` | 系统设置控制台 |
| `http://localhost:5173/audit` | 审计日志页面 |
| `http://localhost:5173/login.html` | 统一登录页面 |
| `http://localhost:8080/healthz` | 后端存活检查 |
| `http://localhost:8080/readyz` | 后端就绪检查 |
| `http://localhost:8080/.well-known/openid-configuration` | OIDC Discovery |

实际地址以 `.env`、`frontend/.env.local` 和 Vite 配置为准。

## 常用后端命令

所有命令均可在项目根目录执行：

```bash
make -C backend fmt       # 格式化 Go 代码
make -C backend vet       # 执行 Go 静态检查
make -C backend migrate   # 执行 MySQL 迁移
make -C backend run-api   # 启动 HTTP API
make -C backend run-worker # 启动异步任务 Worker
make -C backend tidy      # 同步 Go 模块依赖
```

## 配置与安全约束

- 根目录 `.env` 是后端、迁移程序和 Worker 的统一运行配置入口。
- 浏览器登录使用 `HttpOnly` Cookie，不应把访问令牌写入 `localStorage` 或 `sessionStorage`。
- 生产环境必须启用 HTTPS，并将会话 Cookie 的 `Secure` 属性设置为 `true`。
- 密码、AppSecret、客户端密钥、Token、授权码、MFA Secret 和外部身份原始标识不得写入日志或审计明文。
- 外部系统应通过 OAuth/OIDC 或受控应用身份接入，不应复制平台用户密码。
- 外部身份登录只允许关联到已存在且有效的平台账号，不自动创建用户或授予角色。
- 数据库结构变更必须新增版本化迁移，不得直接依赖 GORM 自动迁移。
- 本地上传目录、日志目录、JWT 私钥及运行时数据不得提交到版本库。
- 当前通知投递范围仅包含站内信，不开发邮件和短信发送能力。

## 文档索引

### 接口文档

- [接口文档](api/接口文档.md)
- [OpenAPI 接口规范](api/openapi/平台接口规范.yaml)
- [前后端部署文档](api/前后端部署文档.md)
- [JWT 文档](api/JWT文档.md)
- [数据模型文档](api/数据模型文档.md)

### 架构与开发规范

- [文档说明](docs/文档说明.md)
- [系统架构](docs/系统架构.md)
- [开发计划](docs/开发计划.md)
- [后端实施说明](docs/后端实施说明.md)
- [前后端接口契约](docs/前后端接口契约.md)
- [核心模型与 MySQL 设计](docs/核心模型与MySQL设计.md)
- [后端模型映射](docs/后端模型映射.md)
- [Gin 与 GORM 迁移计划](docs/Gin与GORM迁移计划.md)
- [平台接入说明](docs/平台接入说明.md)
- [前端开发说明](frontend/前端开发说明.md)

### 专项能力文档

- [本地认证开发说明](docs/本地认证开发说明.md)
- [身份管理本地开发说明](docs/身份管理本地开发说明.md)
- [钉钉第三方企业应用扫码登录开发说明](docs/钉钉第三方企业应用扫码登录开发说明.md)
- [跨应用统一登录目标开发说明](docs/跨应用统一登录目标开发说明.md)
- [审计运营深化开发说明](docs/审计运营深化开发说明.md)
- [站内信通知开发说明](docs/站内信通知开发说明.md)
- [可观测性开发说明](docs/可观测性开发说明.md)
- [文件与异步任务开发说明](docs/文件与异步任务开发说明.md)

## 协作开发约定

1. 修改代码前应实际阅读调用链和相关文档，不根据函数名称猜测用途。
2. Go 代码遵循 `gofmt`、清晰命名、分层边界和必要的规范注释。
3. 前后端接口以 OpenAPI、接口契约和实际 Gin 路由为统一依据。
4. 新增功能应同步更新数据模型、迁移、接口文档、环境变量说明及开发文档。
5. 后续业务模块应通过统一身份、授权和审计能力接入，避免重复建设公共能力。
