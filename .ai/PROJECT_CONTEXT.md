# Basic Platform Context

授权执行协调模块 `internal/platform/license/coordination` 与迁移117：独立服务Client、稳定签名快照、全组件Ready/ACK和单调激活状态。机器分发使用独立license.runtime scope；管理读/激活保留用户权限与同源保护。APP_ENV商业部署环境与OAuth应用环境独立绑定，不把production/prod当同一值。实际业务仓库的凭据交付、登记和操作门禁尚未接入，不得据此宣称全系统已强制执行。

商业授权管理入口 `internal/platform/license`，迁移 116 保存部署身份、原始签名许可证、导入事件与已消费恢复凭据。信任公钥编译于 trust 包，不接受许可证或运行配置更换公钥。许可证预览不写业务数据，提交使用行锁/预期版本和显式缩减/替换确认；待生效许可证整份切换。管理页面当前明确标记运行时 NOT_CONNECTED，业务限制与迁移资格/分发/激活仍未接入，不能据此关闭业务 API 或整个 Worker。平台身份与权限链路不受商业期限限制。

基础平台负责租户、身份、登录账号、组织岗位、授权和 Keycloak 接入。`iam_user` 与 `iam_account` 是平台身份事实源；Keycloak 是外部身份与授权投影目标。Keycloak 管理客户端位于 `internal/platform/keycloakauthorization/infrastructure`，身份管理应用位于 `internal/platform/identity/application`。

关键约束：所有查询按租户隔离；关联平台用户的账号会在 Keycloak 管理状态可用时核对外部用户状态；Keycloak 禁用状态会以单向、幂等补偿方式持久化为平台用户及关联账号的 `DISABLED`，外部 `ACTIVE` 不得反向启用平台禁用状态；临时账号到期和临时锁定属于基于时间的有效性，Keycloak 授权投影必须以至少一个有效登录账号为准，并通过 Worker 周期性对账入队，不能依赖账号写事件；Keycloak 凭据不得进入日志或响应。

子系统门户的应用可见性以服务端有效授权为准。平台超级管理员的全应用授权是查询期派生能力，门户查询必须识别其直接、租户级、当前有效的 `platform-super-admin` 绑定，不能要求每个新应用再复制一条用户角色绑定。目录接管（ADOPT）必须在子系统发布角色目录后给执行接管的管理员分配约定初始角色，并持久化接收人和完成时间，保证门户可见性与重试幂等。

子系统探测是租户级接入清单，不是用户门户投影。探测结果必须按租户的 `ACTIVE` 应用编码过滤；应用一旦在任一环境完成登记，其他 Docker 环境标签不得让它再次出现在“未登记子系统”中。草稿或未激活登记不应阻断重新探测和接入。

岗位授权模板是岗位标准应用角色的日常配置入口。“将模板应用到岗位”支持在保存前一键检查多个已选模板的隐性重复：只有应用、角色、授权范围相同且有效期发生交集时才视为重复；不同环境范围或前后不重叠的授权不误报。查重属于只读影响分析，不擅自删除模板或改变岗位映射。

基础平台前端遵循 UniLab UI Kit。全局设计变量的唯一事实源是 `frontend/src/modules/platform/shared/styles/main.css` 的 `:root`；控制台、IAM、门户和设置页面只维护语义别名，不复制基础颜色、字号、圆角或阴影值。登录页使用纯色深色品牌区；子系统门户使用整页展示型大屏，首次默认深色，并支持浅色、深色、跟随系统三态切换和浏览器本地持久化。门户允许低对比度 Canvas 粒子、网格柔光、受限卡片 3D 跟随和短时错峰入场，但必须限制粒子数量和延迟上限、按绘制帧节流、页面不可见时暂停，并遵循 `prefers-reduced-motion` 与精细指针能力；Canvas 配色需要随最终解析主题同步变化。

人员异动以 `iam_personnel_change_request` 为业务事实源，状态只能按领域状态机推进并使用版本号进行并发保护。晋升、降职和调岗真实关闭原任职并建立目标任职；离职必须经过审批和持久化责任交接，所有 `iam_personnel_handover_item` 完成后才能排期，执行时撤销会话并禁用任职、账号和用户；复职恢复既有本地账号、强制下次改密并建立新任职。每次状态推进写入 `iam_personnel_change_transition`，不得以页面状态或自由文本替代审批与交接证据。

生产公开传输由 `deploy/production/bin/public-transport.sh` 统一派生。平台入口与 Keycloak SSO 可以使用不同端口：`PUBLIC_HTTP_PORT` / `PUBLIC_HTTPS_PORT` 控制平台入口，`PUBLIC_SSO_HTTP_PORT` / `PUBLIC_SSO_HTTPS_PORT` 控制 SSO Origin；SSO 端口留空时才继承对应平台端口。所有子系统的 `OIDC_ISSUER` 必须由真实 SSO Origin 派生并与 Keycloak discovery 返回的 issuer 完全一致，不能按平台入口端口推断。

部署兼容约束：CI/CD 与离线安装共用生产 Compose、`deploy-service.sh`、迁移及 readiness 流程；两者仅以不可变镜像的交付/导入方式区分。File Gateway 是独立运行镜像和独立发布摘要，禁止离线部署或 CI/CD 将其折叠进平台 API 镜像。跨系统发布 target 对齐检查见 `deploy/production/bin/test-deployment-parity.sh`，详细契约见 `deploy/production/DEPLOYMENT_COMPATIBILITY.md`。
