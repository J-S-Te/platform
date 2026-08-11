# Keycloak 迁移运行层

本目录只描述 Keycloak 运行层。Keycloak 不会修改基础平台、CRM、合同或项目管理数据库；它使用独立的 MySQL 服务和独立数据卷。本地 `docker-local.sh up --build` 会通过独立 `keycloak` profile 启动它；生产 Compose 默认随主栈启动。

## 本地启动

在 `docker/.env.local`（不要提交）设置 `KEYCLOAK_DB_PASSWORD`、`KEYCLOAK_DB_ROOT_PASSWORD` 和 `KEYCLOAK_ADMIN_PASSWORD`，再运行：

```bash
docker compose \
  --project-name basic-platform-local \
  --file compose.local.yaml \
  --env-file docker/.env.local \
  --profile keycloak \
  up -d keycloak-db keycloak
```

本地地址为 `http://localhost:18090`。这是开发模式，不应直接暴露到公网。

## 生产启动

在未提交的 `.env` 或 CI/CD Secret 中设置：

```text
KEYCLOAK_DB_PASSWORD
KEYCLOAK_DB_ROOT_PASSWORD
KEYCLOAK_ADMIN_PASSWORD
KEYCLOAK_PUBLIC_URL=https://sso.example.com  # 也可以按网关策略使用 http://
```

生产主栈会自动启动 Keycloak：

```bash
docker compose --env-file .env --env-file .release.env \
  --file compose.yaml \
  up -d keycloak-db keycloak
```

`KEYCLOAK_PUBLIC_URL` 必须与入口实际发布的 Issuer 完全一致。是否启用 HTTPS 由网关和
`KEYCLOAK_HTTP_ENABLED` 决定：本地或可信内网可继续使用 HTTP；TLS 在反向代理终止时，容器内
HTTP 仍是正常配置，网关必须传递 `X-Forwarded-Proto/Host/For`。不要把 HTTP 直接暴露到不可信网络。

生产加固、凭据轮换、备份恢复、HA、监控和演练步骤见
[Keycloak 生产运维 Runbook](../docs/keycloak-production-operations.md)。

## 迁移边界

生产应用接入/更新流程会自动同步 Realm、基础平台上游 OIDC、Client 和角色 Claims 映射；生产子系统默认使用 `KEYCLOAK_PUBLIC_URL/<realm>` 作为 issuer，并通过 `SUBSYSTEM_OIDC_BACKCHANNEL_BASE_URL` 访问容器内 Keycloak。需要回滚时，可将 `SUBSYSTEM_DEFAULT_ISSUER_ALIAS` 和 `SUBSYSTEM_OIDC_ISSUER` 切回基础平台 issuer。

## 页面接入原则

子系统接入必须在基础平台“应用接入管理”页面完成，不使用服务器命令新增应用、环境、登录目标或 OAuth Client。当前页面继续由基础平台统一创建应用环境、登录目标和 OAuth Client；Keycloak Realm/Client 的页面化编排需要在该接入流程中作为后续受控步骤接入，不能绕过页面单独执行命令。

Keycloak 运行层只负责认证基础设施；应用编码、环境、登录目标、角色目录和最终授权仍以基础平台页面及其数据库为准。新接入目标默认选择 Keycloak，已有目标需要在页面执行一次更新/重建以切换 issuer。用户、组织、角色与权限的单向映射契约及 Keycloak 故障策略见 [身份与授权映射](../docs/keycloak-identity-mapping.md)。

不要将 Keycloak 的 Realm/Client 角色当作基础平台业务授权的唯一来源。岗位、角色继承、个人例外授权和最终业务权限仍由基础平台计算。

## 配置检查

只检查 Compose 展开结果，不会启动容器：

```bash
docker compose --file compose.local.yaml --profile keycloak config --services
docker compose --file deploy/production/compose.yaml config --services
```

生产输出默认应包含 `keycloak-db` 和 `keycloak`；本地仍需启用 `keycloak` profile。
