# 本地 Docker 与脚本操作说明

> 源码核验：2026-10-13。本次未运行脚本、构建、测试或容器；以下不代表实际环境验收通过。

## 角色和启动条件

由开发／测试运维持 Docker 权限执行；应用接入由对应租户的应用管理员执行。
工作区需相邻的 platform、frontend、contract_management、customer_and_opportunity、project_management、Settlement、data_analysis。
前端构建上下文是相邻 `../frontend`，不是 `platform/frontend`。

保留已有环境文件、数据库密码、签名和字段加密密钥。脚本可以补齐受管非敏感配置，不能笼统理解为“已有文件完全不改”。
已有平台／网关／合同数据卷时缺失原配置会触发安全拒绝，禁止用随机新 Secret 继续连接旧库。
售前交付 Worker 的 `../customer_and_opportunity/.env.presale-worker` 不自动生成。

## 启动与成功判断

在 platform 目录：

```bash
bash scripts/docker-local.sh up
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh verify
bash scripts/docker-local.sh logs api subsystem-provisioner
```

`up` 和 `restart` 同走 start_stack：准备／校验配置、构建镜像、启动数据库／Temporal、迁移、按需初始化管理员，
再启动 Keycloak、File Gateway、平台及配置／目录就绪的子系统。
合同严格配置校验要求有效接入凭据，完全新环境须先核实平台初始化／应用接入顺序；不能跳过凭据校验。
CRM 目录未就绪时相关进程会跳过；Portal、项目、看板还有各自配置和受控接入条件，不能把部分启动当作全系统成功。

管理员人工输入密码；自动化通过安全注入的环境变量转标准输入：

```bash
printf '%s\n' "$ADMIN_PASSWORD" | bash scripts/docker-local.sh up \
  --admin-display-name '平台管理员' --admin-account-name admins --admin-password-stdin
```

禁止用 `--admin-password` 明文参数。`verify` 验证 Nginx、健康路由、匿名访问及登录重定向；
不完成真实登录、权限、业务读写或异步任务验收。成功须补充授权测试账号验收。

## 环境文件和检查副作用

平台、CRM、Portal 配置分别位于 `docker/.env.local`、`docker/.env.customer.local`、`docker/.env.portal.local`；
合同、项目、Settlement、data_analysis 使用相邻仓库的 `.env.local`。实际 Secret 文件保持 0600，不提交。
首次缺失配置可由脚本生成／补齐；接入还会受控写入 OAuth／机器凭据，长期有效密钥不能随意替换。

本地统一入口是 `compose.local.yaml`，生产统一入口是 `deploy/production/docker-compose.yml`。
`ps/logs/verify/stop/down/config` 先执行环境准备，可能创建或补齐本地文件／目录／密钥；不能用于严格零写入审计。
`config` 还做严格校验和目录凭据整理，禁止把它当作无副作用的纯 YAML 检查。

## 定向更新及实际影响

| 命令 | 实际链路与前提 |
| --- | --- |
| refresh-api | 构建平台、补齐平台库／文件网关及其库／受限 Docker 代理、迁移、刷新 API 与 provisioner，并验证 API 到文件网关 readiness |
| refresh-frontend | 重建统一多系统前端／网关；不代表所有子系统已接入 |
| refresh-contract-api | 先整理目录发布凭据并刷新平台 API／provisioner，再构建合同 API／Worker、迁移及目录同步 |
| refresh-customer-api | 构建 CRM／预警 Worker、迁移、目录同步、刷新五类通知 Worker 和前端；不重启独立售前交付 Worker |
| start-presale-alert-worker | 构建预警 Worker，并先执行 CRM 迁移，不是仅启动进程 |
| refresh-portal-api | 需 Portal 已配置；迁移并刷新 Portal、邀请补偿 Worker 及前端 |
| refresh-project-api | 需项目配置就绪；迁移并刷新项目 API，不等于刷新所有 SLA Worker |
| refresh-data-analysis-api | 配置就绪后构建四个 target，迁移并启动 API、聚合／告警 Worker 和 Metabase |
| start-presale-worker | 需额外环境文件；构建交付 Worker／mock，启动 Temporal，校验新鲜 heartbeat；未通过不得开放申请入口 |

这些入口不要求重新登记应用，但有构建、迁移、凭据整理和容器刷新影响。执行前检查对应依赖和维护窗口。
日志先脱敏再分享；失败按具体配置／目录／迁移错误修复，不使用旧镜像伪装更新成功。

## LAN 与远程测试

```bash
bash scripts/lan-access.sh enable --address 192.168.3.11 --port 8081
bash scripts/lan-access.sh status
bash scripts/lan-access.sh disable
```

明确传 IP，Linux 自动探测不能作为可靠前提。该脚本生成 0600 的 `.env.lan`／`.env.customer.lan`，
设置平台 HTTP Issuer，并重建 api、contract-api、contract-worker、customer-api、frontend。
它不覆盖 Portal、项目、看板和 Keycloak 的完整地址／认证配置，不能承诺一条命令完成所有模块远程 SSO。
只用于可信 LAN，地址变更需核对 Issuer、回调和网关；不能直接复制生产。
远程部署步骤见 [服务器本地编排](server-local-orchestration.md)。

## 停止与排错

`stop` 保留容器／卷；`down` 移除容器、orphan 和有关网络，`down --volumes` 删除命名数据卷，仅限明确批准销毁。
File Gateway READY 检查包含数据库和目录；平台更新前先保证网关依赖可用。
门户静态页面存在而 API 未接入是未可用状态，不能强改健康结果。
当前工作区没有 `scripts/seed-local-test-data.sh`，不得按旧手册执行该不存在的入口。

本地 provisioner 经白名单 Socket Proxy 访问 Docker；不要增加 docker.sock 直挂或开放代理公网。

定向刷新平台或合同后端时，会先显式确认 Docker 代理与文件网关依赖，再用 `--no-deps` 更新 API／Agent，避免跳过依赖造成“平台健康但接入失败”。Agent 健康同时检查 Unix Socket 与经受限代理的 Docker 查询；仅有 Socket 不再算健康。代理或网关失败会中止刷新并输出相关服务诊断，不删除环境或业务卷。修复后在原有环境点击“重试”，不要重复新增接入；缺少受审核批准材料时则应先完成下节准备与审核。
`portal-gateway.sh` 的写入及读取采用跨进程锁，超时先检查并发接入／更新，不删除锁绕过。

## 商业授权实例的本地 dev 接入

当前组合根已接入内置组件目录：登记协议 2、catalog version 1，签名快照及商业 JWS 协议仍为 1。新安装及旧 V1 应用通过受控更新登记/桥接；V2 组件身份不再从镜像/coverage digest 派生。实际运行仍需每组件独立机器凭据、状态卷、真实 READY/ACK 和有效许可证，构建成功或部署 READY 不能替代商业权益与业务验收。

`prepare-license/refresh-approvals/recover-license-pins` 是 legacy 批准和固定记录工具，候选会写独立文件，正式批准须显式审核。普通 `up` 不重建已固定服务，`up --build` 只准备候选，不自动采用；新版必须沿既有受控更新由 Agent 交付。不能继续使用“每次 up 都重建、所有接入都必须先准备新版 V1 批准”的旧规则。

启动预检按组件区分登记模式：V1 对照原批准核验镜像；V2 读取 Agent 固定路径的私有运行凭据，核对协议 2、目录版本 1、实例商业环境、服务绑定和受控挂载，不要求旧版批准。两者均检查不可变镜像是否存在；V2 应用还须有必需消费者与迁移镜像。条件邀请补偿组件缺席不会被自动启用，混合登记中的 V1 组件继续核对原批准。这是本地结构/镜像检查，真实凭据、商业权益与当前 ACK 仍由平台及消费者核验；不要清除状态卷、固定记录或关闭许可证门禁。

完整步骤、legacy 固定记录恢复、错误分类和未验证项统一维护于[本地开发商业授权与子系统接入 Runbook](local-dev-license-runbook.md)。生产升级批准与发行完整性另按生产文档执行；本地源码指纹不自动赋予旧安装迁移资格。
