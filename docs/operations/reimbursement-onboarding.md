# 报销子系统受控接入

报销应用码为 `reimbursement`，公开路径 `/reimbursement`，API端口8087，独立Worker健康端口9097。生产描述符为 `deploy/production/subsystems.d/reimbursement-prod.yaml`，正式编排是 `docker-compose.yml`，`compose.yaml`仅为同步参考副本。本地由平台统一Compose托管，默认不启动未接入报销profile，不建立第二套独立业务拓扑。

首次接入、受控更新、ADOPT、RETRY必须交付完整且归属同租户/应用/环境的用途客户端：

| purpose | 精确scope | 运行前缀 |
| --- | --- | --- |
| reimbursement_project_reference_read | reimbursement.project_reference.read | PROJECT_REFERENCE |
| project_reimbursement_cost_write | project.reimbursement_cost.write | PROJECT_SPEND |
| owner_directory_read | owner_directory.read | PLATFORM_OWNER_DIRECTORY |
| audit_ingest | audit.ingest | PLATFORM_AUDIT |
| notification_ingest | notification.ingest | PLATFORM_NOTIFICATION |
| file_gateway_write | platform:file:upload platform:file:bind platform:file:download | FILE_GATEWAY |

目录发布凭据独立交付至PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID/SECRET。两个项目用途ClientID默认分别为 `reimbursement-{env}-project-reference` 与 `reimbursement-{env}-project-cost-writer`。文件用途必须是精确三scope集合，不能套用单scope校验。缺少一次性secret仅允许同一已验身份且现有受控运行文件已有合法secret；否则要求受控RETRY，不能读取持久secret或使用浏览器凭据代替。

项目接收端通过关联元数据解析两个公开ClientID，校验同租户、同环境、来源应用和scope。来源未接入时将REIMBURSEMENT_INTEGRATION_ENABLED置false；只有其中一个身份、重复身份或来源漂移则拒绝发布。接收端绝不取得发送方secret。若项目先于报销接入，需在报销凭据登记后受控更新项目，以启用其接收端；不能靠手填client名称猜测来源。

初始角色固定为 `reimbursement_admin`，不回退通用admin。实际报销目录应先由独立 `/app/reimbursement-authz-catalog publish` 发布，随后才授予初始角色。该角色仅配置管理能力，业务人员、审批、付款和审计角色由管理员另行授权。项目expense_participant应使用SELF应用授权，仅允许费用资格识别，显式项目名单仍由项目管理维护。

四类文件v2用途均绑定reimbursement命名空间：invoice.original允许PDF/OFD；payment.voucher允许PDF/PNG/JPEG；archive.package首发仅JSON清单；export.result允许CSV/XLSX。统一最大20MiB，无隐式ZIP放宽。JSON在底层文件服务也执行完整有界校验，MIME必须同时与扩展名及实际内容相符。下载仍由报销侧先校验业务ACL。

API与Worker分别使用 `runtime/license-reimbursement-api.env` 和 `runtime/license-reimbursement-worker.env` 及独立状态卷。`COMMERCIAL_LICENSE_ENABLED=true` 仅由既有许可投递器写入这些专用文件；普通应用模板与Compose environment不设置该键，也不允许compatibility放行。

迁移顺序为：真实数据库备份及摘要核验成功 → 单次迁移 → 独立目录发布 → API → Worker → 许可与健康预检。`REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED=true` 只通过当次Compose run的 `-e` 参数传递；任何模板或运行文件都不永久保存批准。备份失败不得运行迁移。迁移错误不能自动反向还原数据库，仍遵守现有恢复门禁。

运行配置只由受控接入写入0600文件；源码模板不含密码、ClientSecret或真实租户。此变更只提供接线，不代表已完成真实部署、签发凭据、IdP登录、文件网关联调或运行许可握手。
