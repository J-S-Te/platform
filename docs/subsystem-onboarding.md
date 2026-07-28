# 统一登录目标脚本配置操作说明

> 更新日期：2026-07-28
> 配置方式：仅通过 `scripts/subsystem-onboarding.sh`；基础平台前端不展示统一登录目标配置。

“统一登录目标”属于基础设施接入配置，当前**不在基础平台前端展示**。新增子系统统一使用：

```bash
bash scripts/subsystem-onboarding.sh [认证参数] [接入参数]
```

脚本调用 `POST /api/v1/subsystem-onboarding`。当应用编码尚不存在时，后端会创建 Application、Environment、相对路径 LoginTarget 和 OAuth Client；当同一租户下的应用编码已存在但指定环境尚不存在时，只复用既有 Application 并创建该环境及其 LoginTarget、OAuth Client。脚本不会覆盖已有环境、登录目标或客户端，并把生成的 OIDC 密钥直接交给受控 provisioner。浏览器和脚本输出都不会返回 Client Secret。

## 1. 前置条件

- 基础平台 API 已启动；本地默认地址为 `http://127.0.0.1:8081/api/v1`。
- 运行环境已启用受控子系统 provisioner。本地 `compose.local.yaml` 已启用；生产环境必须先部署并启用对应 provisioner，不能只部署 API 后直接执行脚本。
- `--application-code` 对应的子系统项目目录已存在于 provisioner 配置的项目根目录中。
- 执行账号具备以下权限：
  - `platform:application:create`
  - `platform:application-environment:create`
  - `platform:application-login-target:create`
  - `platform:oauth-client:create`

## 2. 参数映射

| 脚本参数 | 后端字段 | 含义 |
| --- | --- | --- |
| `--application-code` | `Application.Code` | 子系统唯一编码；同时参与项目目录定位和 Client ID 派生 |
| `--application-name` | `Application.Name` | 门户显示名称 |
| `--description` | `Application.Description` | 应用说明；默认 `门户路径接入：<path-prefix>` |
| `--environment` | `Environment.Environment` | `dev`、`test`、`staging` 或 `prod`，默认 `prod` |
| `--public-base-url` | `Environment.BaseURL` | 用户访问的门户 origin，不包含业务路径 |
| `--upstream-url` | `Environment.UpstreamURL` | 门户 Nginx/容器能够访问的子系统内部地址 |
| `--path-prefix` | `Environment.PathPrefix` | 门户路径前缀，默认 `/<application-code>` |
| `--client-type` | `OAuthClient.ClientType` | `confidential` 或 `public`，默认 `confidential` |

后端自动派生，不需要额外参数：

```text
LoginTarget.TargetURI = <path-prefix>/
OAuth redirect_uri     = <public-base-url><path-prefix>/auth/callback
OAuth client_id        = <application-code>-<environment>-web
```

`BaseURL` 与 `UpstreamURL` 必须解耦：前者是浏览器看到的统一入口，后者是网关访问的内部地址。不要把容器内的 `localhost` 当作其他容器的 Upstream。

## 3. 本地合同管理系统示例

合同系统项目目录名为 `contract_management`，本地统一前端/API 入口为 `http://localhost:8081`，合同后端在 Compose 网络内的服务地址为 `http://contract-api:8081`：

```bash
cd /Users/yglf/GOPATH/src/Unified_Identity_Authentication_Platform/platform

bash scripts/subsystem-onboarding.sh \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --environment prod \
  --public-base-url http://localhost:8081 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management \
  --client-type confidential \
  --account admin
```

脚本会安全交互读取密码。用于受控 CI 时可以从标准输入传入：

```bash
printf '%s\n' "$PLATFORM_ADMIN_PASSWORD" | bash scripts/subsystem-onboarding.sh \
  --password-stdin \
  --account admin \
  --application-code contract_management \
  --application-name '合同管理系统' \
  --public-base-url http://localhost:8081 \
  --upstream-url http://contract-api:8081 \
  --path-prefix /contract_management
```

也可通过 `--cookie-file FILE` 复用已有平台会话。脚本会把 Cookie 复制到私有临时目录，不覆盖调用者文件。

## 4. 单终端登录注意事项

平台禁止同一账号同时保持多个终端会话。脚本使用账号口令登录时，会在执行结束后自动调用 `/auth/logout`，避免运维脚本遗留会话导致管理员之后无法登录。

如果管理员账号已有会话，优先在原终端正常退出。只有明确要撤销原会话时才使用：

```bash
--replace-existing-session
```

该参数会使原终端会话立即失效，不应作为默认配置。

## 5. 与 portal-gateway.sh 的边界

`scripts/portal-gateway.sh` 是低层 Nginx 路由维护工具，只处理路径到 Upstream 的映射，不创建 Application、Environment、LoginTarget 或 OAuth Client。

新增子系统不要只执行 `portal-gateway.sh add`；应执行 `subsystem-onboarding.sh`，让后端受控 provisioner 完成配置写入、子系统启动和网关更新。`portal-gateway.sh` 仅用于故障排查、删除路由或受控运维。

## 6. 常用检查

```bash
bash scripts/subsystem-onboarding.sh --help
bash scripts/portal-gateway.sh list
bash scripts/docker-local.sh ps
```

脚本是创建/新增环境流程，不是更新或覆盖流程。相同租户下已有应用编码时，若指定环境尚不存在，脚本会复用该 Application 并新增该环境；这适用于已预置 `contract_management` 后再创建 `prod` 环境。重复使用相同的应用编码和环境、登录目标、路径前缀或 Client ID 时，平台会返回 `409`，不会覆盖任何现有配置。需要修改现有接入配置时，应走受控变更流程，不能通过重复执行绕过唯一性约束。
