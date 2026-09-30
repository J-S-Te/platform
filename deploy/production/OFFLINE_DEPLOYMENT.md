# 离线统一部署流程

> 本文是流程摘要。可直接照做的唯一详细手册为
> [OFFLINE_RUNBOOK.md](OFFLINE_RUNBOOK.md)，其中逐步注明执行机器、目录、身份、预期输出和失败处理；
> 服务器验收证据清单见 [ACCEPTANCE_CHECKLIST.md](ACCEPTANCE_CHECKLIST.md)。本文与手册不一致时停止部署并修正文档/脚本，不自行选择较宽松的步骤。

生产只维护 `docker-compose.yml`，部署目录默认 `/opt/unified-identity-platform`。镜像导入和容器启动是两个独立动作。

## 1. 构建和安装部署资产

构建机从当前源码构建 linux/amd64 离线包，版本必须唯一。公共镜像包、平台、前端和各业务镜像分别交付；无需 CI 才能构建。

```bash
./bin/build-offline-packages.sh --version <版本> --component all --output <交付目录>
```

版本名对应不可变交付：同一输出目录中已有同名且内容不同的包时，构建器会拒绝覆盖，必须改用新版本。镜像包与资产包均先在输出目录的隐藏临时文件中完成压缩、结构校验和 SHA-256 计算，再以原子重命名提交；失败或信号退出会清理临时内容。部署资产使用逐文件静态白名单，`.env*`、私钥、数据库导出及其他未登记文件默认不会进入归档。

将镜像包、Ubuntu/amd64 宿主依赖包、部署资产包、对应 `.sha256`、
`BUILD_INFO.txt` 以及包外独立的两个安装入口及 sidecar 一并复制到服务器。
宿主依赖包与构建机 Ubuntu `VERSION_ID`/代号严格绑定；必须与部署机完全一致。
先在包目录检查总清单和两个安装入口，再运行：

```bash
sha256sum --check SHA256SUMS
sha256sum --check install-host-dependencies.sh.sha256
sha256sum --check install-assets.sh.sha256
sudo ./install-host-dependencies.sh host-dependencies-ubuntu-*-linux-amd64.tar.gz
sudo ./install-assets.sh deployment-assets-<版本>.tar.gz /opt/unified-identity-platform
cd /opt/unified-identity-platform
sudo ./bin/deploy.sh configure
```

宿主依赖归档包含 `jq/curl/ca-certificates/tar/gzip/coreutils` 以及部署、备份、
恢复路径需要的其他 APT 依赖，并通过只指向已校验归档的本地 `file:` 仓库安装，
不会隐式访问公网。Ubuntu 基础系统仍需预先提供引导所需的
`bash/tar/gzip/sha256sum/dpkg/apt-get`；Docker Engine 与 Compose plugin 仍需单独预装，
安装器不会自动安装或替换 Docker。

安装器保护既有 `.env`、`.release.env`、runtime、数据和备份。升级时保留当前 `docker-compose.yml`，将新模板写为 `docker-compose.yml.dist` 供管理员合并，避免覆盖已注释的部署范围。

资产安装会先完成全量备份，再写入 `runtime/.assets-install-transaction` 事务标记。若磁盘、复制或进程中断留下标记，`deploy.sh` 和下一次资产安装都会拒绝在混合版本上继续；先执行显式恢复：

```bash
sudo ./bin/install-assets.sh --recover /opt/unified-identity-platform
```

恢复只处理该事务登记的静态资产，运行配置、数据库卷和备份不会被删除。恢复成功后再重新安装同一份已校验资产包。

## 2. 选择模块与初始化

`docker-compose.yml` 默认包含全部系统。公共平台区必选；完整注释 BEGIN SUBSYSTEM / END SUBSYSTEM 之间的服务即可移除该模块。七个块是 contract、project、customer、portal、settlement、data-analysis、monitoring。顶层未使用卷声明可以保留。

部署范围仅由 Compose 实际服务决定，不再维护 profiles 或另一份启用列表。配置向导只准备启用模块的 runtime。公共 contract-mysql 保留原服务名和卷，用作 Temporal 共享数据库；注释合同业务不会停止它。

configure 询问 IP、端口、账号、时区等，生成并保留所需随机密钥和密码。runtime 目录 0700、凭据文件 0600。重复配置保留已有密钥；不手填 PENDING 平台凭据。平台生成 OIDC、授权目录、通知、人员目录、审计和 File Gateway 调用凭据。

## 3. 导入并部署基础平台

```bash
sudo ./bin/deploy.sh import packages/common-infrastructure-linux-amd64.tar.gz
sudo ./bin/deploy.sh import packages/platform-backend-<版本>-linux-amd64.tar.gz
sudo ./bin/deploy.sh deploy platform
sudo ./bin/deploy.sh import packages/frontend-<版本>-linux-amd64.tar.gz
sudo ./bin/deploy.sh deploy frontend
sudo ./bin/deploy.sh verify
docker ps -a
```

导入要求 `.sha256` 只含一条摘要，且文件名必须与当前包完全相同；随后校验包 SHA-256、包内 images.tar 摘要、登记名称和 linux/amd64 架构，再 docker load 并登记 digest。每次成功导入会原子更新该组件的 `*.latest` 指针，部署使用最后一次明确导入的版本，不按版本字符串字典序猜测“最新”。导入不会自动启动。

平台发布分阶段执行 platform-key-init、公共数据库健康等待、平台迁移、Keycloak/Temporal、网关、socket proxy、Agent、API/Worker，再初始化或验证管理员。前端单独发布，HTTPS 和排空使用同一服务与 runtime/public-tls 受管目录，不加载覆盖文件。

`deploy.sh install` 可扫描 packages 自动完成公共平台和前端，再为 YAML 保留且已提供镜像包的子系统准备候选。被注释模块不会要求镜像、runtime 或迁移。

## 4. 平台接入业务子系统

以 CRM 为例：

```bash
sudo ./bin/deploy.sh import packages/customer-opportunity-backend-<版本>-linux-amd64.tar.gz
sudo ./bin/deploy.sh prepare customer-opportunity
# 在基础平台页面探测并采用 customer_and_opportunity/prod
sudo ./bin/deploy.sh status customer-opportunity
sudo ./bin/deploy.sh continue customer-opportunity
sudo ./bin/deploy.sh verify customer-opportunity
```

其他命令参数为 customer-portal、contract、project、settlement、data-analysis。prepare 创建候选，不接受业务请求；实际数据库备份、迁移、凭据下发和启动由平台受控接入完成。continue 是已接入后的验收门禁，不绕过接入。

未注释只表示允许部署。缺失凭据或迁移失败必须停止，不使用假凭据启动。数据分析启动前幂等初始化 Metabase 数据库；门户邀请补偿在 CRM 未启用/未接入时跳过。

## 5. 重启、裁剪和停用

```bash
sudo ./bin/deploy.sh start
sudo ./bin/deploy.sh resume contract
sudo ./bin/deploy.sh status
sudo ./bin/deploy.sh disable contract
```

start 使用当前 release 版本分阶段启动公共平台及已接入模块；首次初始化管理员仍使用 deploy platform。resume 只操作已接入单系统，完成依赖健康等待、备份、迁移、必要目录同步和启动。保留既有 workflow namespace、队列和 Worker 版本策略。

已运行模块被注释后，旧容器不会自动停止。启动/状态检查列出差异并提示 disable 命令。disable 仅按项目/服务标签停止容器，保留卷与凭据，不执行 down 或 remove-orphans。重新启用需取消注释，再 resume。

注释只能保证其他服务可启动，不能保证跨系统业务完整：合同缺失影响 CRM 合同交接；项目缺失影响合同项目交接及门户查询；CRM 缺失影响权威数据及邀请补偿；门户、结算、看板缺失分别影响客户自助、开票、统计报表。网关动态 DNS 允许缺失后端，相关请求返回不可用，异步任务沿用重试。

## 6. 运维与排错

```bash
sudo ./bin/deploy.sh doctor
sudo ./bin/deploy.sh backup
sudo ./bin/deploy.sh restore --service <服务> --backup <文件> --verify-only
sudo ./bin/deploy.sh restore --service <服务> --backup <文件> --confirm RESTORE_MYSQL_SERVICE
docker logs --tail 200 uip-platform-api
docker inspect uip-platform-api
```

`backup` 生成包含数据库、File Gateway 一致性批次、运行配置、签名密钥、受管 TLS 和版本清单的受限灾备介质；生产环境推荐直接使用 `bin/backup-all.sh --encryption-key-file <独立保管的密钥文件>`。完整校验、空目录重建演练、共享 Temporal 数据库维护确认和 File Gateway 同批次恢复见 [BACKUP_RECOVERY.md](BACKUP_RECOVERY.md)。恢复脚本持有统一部署锁并扫描已裁剪但仍运行的旧容器，不能用删除卷绕过维护门禁。

业务升级先导入镜像，再 upgrade 暂存，最后由平台受控更新。常驻容器具备健康检查、日志轮转和资源限制。失败迁移保留容器供 docker ps -a / docker logs 检查；不要删除数据库卷来解决配置或认证问题。

验收必须包括全系统、单块及多块裁剪、平台独立启动、重复启动、故障迁移、Worker 重启和工作流恢复。Compose 解析与脚本测试不等于真实业务验收；真实登录、恢复演练需要可用镜像、凭据和隔离服务器环境。
