# 新服务器构建与离线部署验收清单

本清单是 `OFFLINE_RUNBOOK.md` 的验收配套文件。Runbook 说明如何部署；本文件说明如何证明部署确实可交付。执行人必须填写证据和结果，不能把容器 `Running`、HTTP 200、脚本单元测试或文档推演等同于真实业务通过。

## 1. 适用范围和硬边界

- 目标机：本轮为 `192.168.31.153`，实际报告可只写资产编号或对地址脱敏。
- 原服务器 `192.168.3.38` 不在本轮范围，禁止登录或操作。
- 不重启宿主机、不修改 root 密码、不删除验收前已存在的数据。
- 密码只在交互提示或受限的 Secret 文件中输入，禁止放在命令参数、Shell 历史、证据、截图和报告中。
- 首次基线发现已有容器、卷、监听端口、部署目录或业务数据时，停止“空白机”假设，保留原状并登记为阻塞。
- 所有破坏性演练仅针对本轮创建且有明确标签、目录和备份的测试资源。
- 本轮没有执行的宿主机重启、长期稳定性和外部系统联调必须标为 `NOT RUN` 或 `BLOCKED`。

建议会话开始时关闭 Shell 历史写入；结束前恢复原值，不要删除原有历史：

```bash
OLD_HISTFILE=${HISTFILE-}
unset HISTFILE
# ...本轮交互操作...
HISTFILE=$OLD_HISTFILE
```

## 2. 结果状态和证据规则

每项只能使用以下状态：

- `PASS`：在本轮目标机真实执行，判据全部满足并有脱敏证据。
- `FAIL`：已执行但判据不满足。
- `BLOCKED`：因凭据、外部依赖、授权或环境条件不足无法执行。
- `NOT RUN`：明确不在本轮授权或计划内。
- `N/A`：最终交付确实不包含该模块；必须写明裁剪依据。

证据目录必须为 `0700`，文件必须为 `0600`。允许记录：命令、开始和结束时间、退出码、镜像 digest、容器健康、重启次数、HTTP 状态、测试记录 ID、耗时。禁止记录：密码、Cookie、Authorization Header、Token、Client Secret、私钥、容器完整环境变量、完整数据库行、包含个人信息的请求或响应、未经审查的原始日志。

只读证据采集入口：

```bash
cd /opt/unified-identity-platform
sudo ./bin/acceptance-evidence.sh postdeploy \
  --output /root/uip-acceptance/evidence-postdeploy-$(date -u +%Y%m%dT%H%M%SZ)
```

采集器只负责技术证据，不能自动把人工业务项判为通过。

## 3. 验收报告首页

复制以下内容到本轮报告并填写：

```text
发布版本：
源码提交/源码目录 SHA-256 清单：
构建机资产编号：
部署机资产编号：
构建开始/结束 UTC：
部署开始/结束 UTC：
执行人/复核人：
交付目录 SHA256SUMS 摘要：
部署 Compose SHA-256：
验收范围：platform/frontend/CRM/portal/contract/project/settlement/data-analysis
技术验收：PASS / FAIL / BLOCKED
业务验收：PASS / FAIL / BLOCKED
恢复验收：PASS / FAIL / BLOCKED
RTO 实测：
RPO 实测：
宿主机重启：NOT RUN
长期稳定性：NOT RUN
未解决项及影响：
保留资源：
清理结果：
```

## 4. 阶段 A：变更前只读预检

执行位置：目标服务器任意只读目录；身份：root；时间：安装任何依赖、复制源码或创建容器之前。

```bash
install -d -m 700 /root/uip-acceptance
/path/to/acceptance-evidence.sh baseline \
  --output /root/uip-acceptance/evidence-baseline-$(date -u +%Y%m%dT%H%M%SZ)
```

若采集脚本尚未传入，可只读执行并把输出保存在 `0600` 文件中：

```bash
uname -a
cat /etc/os-release
df -hT
df -hi
free -h
ss -lntH
docker ps -a 2>/dev/null || true
docker volume ls 2>/dev/null || true
docker network ls 2>/dev/null || true
```

通过判据：

- Linux `x86_64`/`amd64`；时间及时区明确，NTP 状态已记录。
- `/opt`、Docker data-root、`/tmp` 和备份目录空间及 inode 满足 Runbook 预算。
- 已有容器、卷、网络、监听端口和 `/opt/unified-identity-platform` 已完整盘点。
- 若机器并非空白，报告列出资源所有者和隔离方案，未经确认不得清除。
- SSH 和内网管理路径可达，后续网络演练有第二会话或控制台兜底。

## 5. 阶段 B：联网构建机门禁

执行位置：源码根目录；身份：具有 Docker 权限的构建用户，系统包安装阶段才使用 root。

```bash
platform/deploy/production/bin/acceptance-evidence.sh build-gate \
  --output /root/uip-acceptance/evidence-build-gate-$(date -u +%Y%m%dT%H%M%SZ)
```

通过判据：

- `linux/amd64`，Docker Engine、Buildx、Compose v2、Bash 4.3+、`jq`、GNU `tar/find/stat/coreutils` 可用。
- 构建版本唯一且不可复用，例如 `20260927T153000Z-<短提交号>`。
- 源码目录没有生产 `.env`、私钥、数据库和业务文件。
- Dockerfile 的 `FROM` 版本固定，构建上下文和 `.dockerignore` 与 `COPY` 路径一致。
- 代理或镜像加速仅作为构建机配置，不写入运行镜像、Compose 或交付资产。

记录以下信息，不记录 Registry 密码：

```bash
git rev-parse HEAD 2>/dev/null || true
docker version
docker buildx version
docker compose version
sha256sum platform/deploy/production/docker-compose.yml
```

## 6. 阶段 C：全部自研镜像构建与打包

执行位置：源码根目录；身份：构建用户。

```bash
export OFFLINE_RELEASE_VERSION='<唯一版本>'
platform/deploy/production/bin/build-offline-packages.sh \
  --component all \
  --version "$OFFLINE_RELEASE_VERSION" \
  --output "/root/uip-build/$OFFLINE_RELEASE_VERSION"
```

必须真实构建而不是只检查已有 tag。逐包记录：组件、版本、镜像 tag、镜像 config digest、平台、压缩包大小、外层 SHA-256。对多镜像组件逐个记录镜像摘要。Docker classic `manifest.json` 和 containerd/OCI `blobs/sha256/...` 的 config 路径不同；验收依据必须是归档内实际 config blob 摘要，不能把 image index、manifest list 和 config digest 混为一个字段。

构建通过判据：

- common、platform、frontend、CRM、portal、contract、project、settlement、data-analysis、host-deps 和 assets 均存在。
- 全部自研运行镜像实际架构为 `linux/amd64`。
- 宿主依赖包与目标 Ubuntu `VERSION_ID`、代号和 amd64 架构一致，包含 `jq/curl/ca-certificates/tar/gzip/coreutils` 及完整依赖闭包；不同 OS 包必须拒绝安装。
- 每个 `*.tar.gz.sha256` 只包含当前同名包一行；两个独立安装入口 sidecar 均只绑定当前脚本；总 `SHA256SUMS` 覆盖最终交付目录全部包、安装入口和 `BUILD_INFO.txt`。
- 同版本增量构建必须与既有 `BUILD_INFO.txt` 的发布版本、平台、源码输入指纹和工具链完全一致。镜像 config 摘要/标签集合一致或资产逐文件内容一致时可安全复用原产物；语义内容不同必须失败，已有正确产物不被覆盖。
- 构建失败或中断后没有被当作完成产物的临时包，重新执行能继续或安全重建。
- 资产包包含 `OFFLINE_RUNBOOK.md`、本验收清单、宿主依赖构建/安装脚本和证据采集器，安装器允许同一资产集合并支持事务恢复；包外另有可直接校验运行的 `install-host-dependencies.sh` 与 `install-assets.sh`。

建议记录每个组件的开始、结束和耗时；网络慢不是通过条件，最终必须有完整产物。

### 6.1 有界磁盘不足演练

不要填满宿主分区。使用专用 loop 文件创建有上限的临时文件系统，且只对一次可重建的测试版本执行：

```bash
DISK_TEST_ROOT=/var/lib/uip-acceptance-disk-test
DISK_TEST_IMAGE=$DISK_TEST_ROOT/limited.img
DISK_TEST_MOUNT=$DISK_TEST_ROOT/mnt
install -d -m 700 "$DISK_TEST_MOUNT"
truncate -s 96M "$DISK_TEST_IMAGE"
mkfs.ext4 -q "$DISK_TEST_IMAGE"
DISK_TEST_LOOP=$(losetup --find --show "$DISK_TEST_IMAGE")
mount "$DISK_TEST_LOOP" "$DISK_TEST_MOUNT"
```

将单组件测试构建的 `--output` 指向该目录，预期构建因空间不足失败；确认正式输出目录中既有包和 SHA 未变化。无论成功失败都执行：

```bash
umount "$DISK_TEST_MOUNT"
losetup -d "$DISK_TEST_LOOP"
rm -f "$DISK_TEST_IMAGE"
rmdir "$DISK_TEST_MOUNT" "$DISK_TEST_ROOT" 2>/dev/null || true
```

若没有 loop/mount 授权，标记 `BLOCKED`，不能用向真实分区写满数据替代。

## 7. 阶段 D：隔离交付与完整性验证

部署验收目录只能接收构建产物、总 SHA 清单和独立安装入口，不能直接引用源码目录或构建缓存。

```bash
cd /root/uip-delivery/<版本>
sha256sum --check SHA256SUMS
/path/to/acceptance-evidence.sh delivery-gate \
  --package-dir "$PWD" \
  --output /root/uip-acceptance/evidence-delivery-$(date -u +%Y%m%dT%H%M%SZ)
```

通过判据：

- 两端 `SHA256SUMS` 文件本身摘要一致；全部条目为 `OK`。
- 目录无符号链接、设备文件、套接字或路径逃逸。
- 无 `.env`、私钥、凭据、数据库、业务附件或备份。
- 删除源码访问权限、切换到独立目录后仍能取得安装入口、手册和全部包。
- 将一个包复制到隔离临时目录并修改一个字节后，SHA 校验和导入都必须拒绝；不得破坏原包。

## 8. 阶段 E：离线部署隔离设计

### 8.1 构建缓存隔离

同机测试不能通过清空全局 `/var/lib/docker` 模拟空白机。应使用专用 Docker data-root/exec-root/socket 和独立 Compose project/network；原 Docker daemon 保持不变。专用 daemon 的完整启动参数、数据目录、PID、socket 和停止清理命令必须进入报告。

最低判据：

- 专用 data-root 初始为空。
- 部署命令的 `DOCKER_HOST` 明确指向专用 socket。
- 部署只读隔离交付目录，不读取源码目录。
- 验收完成前不删除专用 data-root；结束时登记保留或清理决定。

如果无法安全启动专用 daemon，则同机“全新离线机”验收标为 `BLOCKED`，不能把使用构建缓存的结果写成通过。

### 8.2 有自动恢复的容器出口限制

出口限制只应用到验收 Compose 专用 bridge 子网，不修改 host `OUTPUT`，因此不影响 SSH 和宿主机内网管理。执行前必须：

1. 打开第二个 SSH 会话或确认带外控制台；
2. 记录 `iptables-save`/`nft list ruleset`，但证据中移除无关地址；
3. 创建只删除本轮唯一链和跳转的回滚脚本，权限 `0700`；
4. 用 `systemd-run --on-active=20m` 安排自动回滚，确认 timer 已加载；
5. 从 Compose 网络 inspect 得到专用 CIDR，禁止凭猜测填写；
6. 只允许 loopback、连接跟踪已建立流量和经批准的 RFC1918 内网网段，拒绝该测试 CIDR 到公网；
7. 验证完成后主动运行回滚脚本并取消 timer，再比较规则差异。

判据：

- 测试容器访问 Docker Hub/公网地址失败。
- 测试容器访问 Compose 内部服务和经批准的内网地址成功。
- 本地 Registry `127.0.0.1:5000` 可用，所有运行镜像均能从本次导入内容解析。
- SSH 两个会话始终保持，宿主机其他工作负载网络不受影响。
- 自动和主动回滚均只删除本轮规则；最终无残留链或 timer。

注意：`DOCKER-USER` 只约束容器转发，不约束宿主机 Docker daemon 自身拉取。要证明部署没有暗中访问公网，必须同时满足“专用空 data-root + 所有 Compose 解析镜像均来自已校验包/本地 Registry + daemon 审计无外部 Registry 请求”。只做容器 `curl` 失败不足以证明离线包完整。

## 9. 阶段 F：空目录安装与配置

执行位置：隔离交付目录；身份：root。

逐项验收：

| 项目 | 判据 |
| --- | --- |
| 空目录首次安装 | 目标目录由安装器创建；文件类型和权限正确；没有读取源码目录 |
| 重复安装 | 相同资产幂等；用户的 Compose 裁剪保留；新模板写入受控 `.dist` |
| 安装中断 | 事务标记存在时部署入口拒绝继续；`install-assets.sh --recover` 恢复一致版本 |
| 重复配置 | 已生成密码和密钥不被重置；只询问允许变更的公开参数 |
| 配置中断 | 临时文件不被当作正式 `.env`；重试后文件为 `0600` |
| 端口占用 | 预检明确列出冲突并停止，不杀死未知进程 |
| 网络冲突 | Compose project/network 冲突明确失败，不复用未知网络 |
| 防火墙 | 只提示或操作明确端口；不自动启用/关闭整套主机防火墙 |
| 时区 | 输入值合法，容器和应用时间一致 |
| 密钥 | 首次生成、权限、持久化和灾备均有证据；报告不出现密钥内容 |

配置后运行：

```bash
cd /opt/unified-identity-platform
sudo ./bin/deploy.sh doctor
sudo ./bin/deploy.sh status
```

## 10. 阶段 G：镜像导入和公共服务启动顺序

严格按 Runbook 执行。每个阶段失败必须停止，不能继续启动依赖服务。

推荐检查顺序：

1. 本地 Registry 和 common 基础镜像；
2. 平台签名密钥初始化；
3. 公共 MySQL 及健康检查；
4. 平台迁移和管理员初始化；
5. Keycloak 数据库、Keycloak、Issuer discovery；
6. contract-mysql 和共享 Temporal；
7. File Gateway 数据库、迁移、对象目录和 API；
8. Docker socket proxy 与 subsystem-provisioner；
9. platform-api、platform-worker；
10. frontend；
11. 已在平台受控接入的业务系统。

技术通过判据：

```bash
sudo ./bin/deploy.sh doctor
sudo ./bin/deploy.sh verify
sudo ./bin/deploy.sh status
```

- API、File Gateway、Keycloak、前端和受控 Agent 为 healthy。
- `platform-worker` 在观察窗口内运行、健康且重启次数不增长。
- Agent 容器内 `.release.env`、Compose 摘要与宿主机一致。
- File Gateway 匿名受保护接口返回 401/405，而不是 404/502/504。
- Temporal 真正接受连接；不能只看容器 Running。
- 前端从部署机本地入口和另一台内网客户端都可访问；安全组/边界防火墙另有证据。
- 失败迁移容器保留且日志可定位，成功后本次迁移容器被精确清理。

## 11. 阶段 H：业务系统受控接入

每个启用子系统必须单独执行和记录：

```text
import 包
→ prepare <组件>
→ 平台页面采用 <application_code>/prod
→ status <组件>
→ continue <组件>
→ verify <组件>
```

推荐顺序：CRM → 客户门户 → 合同 → 项目 → 结算 → 数据分析。门户依赖 CRM；合同与项目共享 Temporal 及 contract-mysql 相关能力，裁剪时不得误停共享依赖。

| 组件参数 | application code | 最低业务验收 |
| --- | --- | --- |
| `customer-opportunity` | `customer_and_opportunity` | 登录；创建/查询一条验收商机；相关通知 Worker 处理一次任务 |
| `customer-portal` | `customer_portal` | 客户登录；读取授权范围内数据；邀请补偿 Worker 在 CRM 可用时完成一次任务 |
| `contract` | `contract_management` | 登录；创建/读取测试合同；审计事件可查询 |
| `project` | `project_management` | 登录；创建/读取测试项目；SLA notifier 执行一次可观察任务 |
| `settlement` | 对应清单值 | 登录；创建/读取测试结算记录；Worker 处理一次任务 |
| `data-analysis` | 对应清单值 | 登录；真实查询得到预期统计；aggregation/alert worker 有成功周期 |

所有测试记录使用唯一前缀 `ACCEPTANCE-<UTC>-`，以便恢复后确认和最终清理。缺少真实接入凭据时，系统必须停在受控阶段且业务容器不得启动；这项安全行为可以 `PASS`，业务验收仍为 `BLOCKED`。

## 12. 阶段 I：真实业务判据

对平台和每个启用子系统至少验证：

1. 正确用户登录成功，错误密码失败且不泄漏账户是否存在；
2. 未授权用户不能访问另一租户/角色的数据；
3. 创建一条带唯一标识的测试记录，读取内容一致，更新产生正确版本或审计；
4. 如果允许删除，删除仅作用于测试记录；否则执行合法状态流转；
5. 文件上传后能下载并核对 SHA-256，越权下载失败；
6. 至少一个核心异步任务从入队到成功完成，重试次数和最终状态可观察；
7. 审计记录包含非敏感主体、动作、资源和 trace/request ID；
8. 页面不可用时显示明确提示，不返回 Nginx 504 HTML 作为业务错误；
9. API 超时、依赖失败和重复请求不会错误返回成功或生成重复业务数据。

报告只记录测试记录 ID、状态、响应码、耗时和脱敏截图；不要复制响应中的个人信息或 Token。

## 13. 阶段 J：裁剪、故障和升级回归

### 13.1 Compose 裁剪

- 自动解析全部可选块组合，确保 Compose 可解析。
- 每个模块单独注释后，`compose-scope` 显示能力影响。
- 注释本身不停止旧容器；必须用 `deploy.sh disable <组件>` 显式停用并保留数据。
- 平台和前端在没有任一业务模块时仍健康。
- 停用合同不得停止项目仍需的共享 Temporal/contract-mysql。
- 上游缺失时前端返回明确不可用状态，不返回 504 网关 HTML。

### 13.2 可逆故障演练

每项先保存 `status` 和 restart count，演练后必须恢复并重新执行 `doctor/verify`：

- 错误密码：只修改隔离副本或本轮测试数据库凭据，不覆盖未知持久卷中的真实密码。
- 迁移失败：只使用专门的失败迁移测试包；没有该包则标记 `BLOCKED`，禁止篡改正式 migration。
- 前端入口不可用：停止本轮 frontend，确认外部探针失败且发布不误报；立即按 Compose 恢复。
- Worker 重启：重启一个指定 Worker，验证恢复健康、任务不丢失且幂等。
- 安装中断：仅在临时目标目录执行，确认事务恢复；禁止在已运行目录注入 kill。
- 损坏镜像包：仅破坏副本，确认 SHA 和 import 都拒绝。

### 13.3 单系统升级与回滚

- 使用新唯一版本导入并执行 `upgrade <组件>`；平台页面受控更新。
- `.release.env`、Agent 视图和运行容器最终为同一不可变 digest。
- API 及所有该系统 Worker 都切换到新镜像。
- 主动注入健康失败时，发布返回非零并恢复 API、Worker 和上一 digest；失败容器证据保留。
- 数据库迁移不承诺自动反向回滚，报告必须写明 schema 兼容策略。

## 14. 阶段 K：备份和恢复演练

仅使用本轮创建的测试记录；恢复前创建恢复点，恢复过程不得作用于验收前已有数据。

### 14.1 记录 RPO/RTO

- `T0`：开始停止写入/进入维护窗口。
- `Tbackup`：备份完成且校验通过。
- `Tfailure`：模拟故障时间。
- `Trestore-start`：开始恢复。
- `Tservice-ready`：技术健康恢复。
- `Tbusiness-ready`：测试登录、读写、文件和异步任务全部恢复。
- `RTO = Tbusiness-ready - Tfailure`。
- `RPO`：恢复点之后、故障之前最大可丢失业务时间；用带 UTC 的测试记录验证，不能只写理论值。

### 14.2 数据库恢复

- 恢复前列出的所有 API、Worker、迁移任务和遗留 orphan writer 均停止。
- `contract-mysql` 恢复明确包含共享 Temporal 维护确认。
- 恢复命令持有统一部署锁，并拒绝并发发布。
- 恢复后 API、Worker、Temporal 和业务读写全部复验。

### 14.3 File Gateway 一致性恢复

- 数据库和对象文件必须来自同一批次，禁止单独恢复 `file-gateway-mysql`。
- 备份窗口停止全部写入者；临时上传是否排除与产品策略一致。
- 恢复后对象文件属主为运行 UID/GID，目录和文件权限符合 Runbook。
- 上传、下载、SHA-256、绑定关系和无 READY 孤儿文件全部通过。
- 旧对象目录保留到业务验收完成后才由人工处理。

### 14.4 配置、密钥和灾备

```bash
sudo ./bin/backup-all.sh
sudo ./bin/backup-all.sh --verify-only --backup <本轮备份批次>
sudo ./bin/backup-all.sh --rebuild-drill \
  --backup <本轮备份批次> \
  --drill-root <空的隔离目录>
```

通过判据：数据库、File Gateway、`.env`、`.release.env`、Compose、runtime、清单、平台签名密钥、TLS 材料和版本清单完整；加密密钥与介质分开保存；空目录重建演练不修改在线系统。

## 15. 阶段 L：收尾

- 主动撤销测试网络规则，取消自动回滚 timer，确认无残留。
- 删除有界磁盘测试的 mount、loop 和文件；确认没有挂载残留。
- 清理测试业务记录前先确认恢复验收已结束。
- 列出保留的镜像、容器、卷、网络、备份、交付包和证据目录；未知资源不删除。
- 再次采集容器、卷、网络、监听端口并与基线比较。
- 证据目录打包前人工检查敏感信息；报告中使用资产编号和脱敏标识。
- 宿主机重启继续标记 `NOT RUN`；容器 restart 不得替代宿主机重启验收。

## 16. 当前脚本覆盖矩阵

| 用户要求 | 现有自动化覆盖 | 仍需真实人工/服务器验收 |
| --- | --- | --- |
| 包 SHA、元数据、架构、config digest | 构建器、导入器、metadata 回归 | 全组件真实构建及独立交付目录复验 |
| 空目录安装、事务恢复 | 安装器与资产安装回归 | 目标 Linux 空目录照册执行和中断演练 |
| 平台启动顺序、失败停止 | `start-enabled.sh`、发布回归 | 空 data-root 离线真实启动 |
| Agent 配置一致性、统一锁 | refresh helper、发布可靠性回归 | 原子替换后的真实容器摘要核对 |
| 技术健康 | `doctor`、`verify`、容器 healthcheck | 外部客户端、安全组和真实依赖 |
| 业务读写和权限 | 无法由通用部署脚本安全自动生成 | 必须用授权测试账号逐系统执行 |
| Worker 真实任务 | health/restart 可检查 | 必须创建测试任务并观察最终状态 |
| 单系统升级/回滚 | deploy/lifecycle 发布流程 | 新旧真实镜像和故障注入 |
| 数据库写入者隔离 | restore 脚本检查及安全回归 | 本轮测试数据的真实恢复窗口 |
| 网关 DB/文件一致性 | 专用 backup/restore/drill | 真实上传对象恢复和下载校验 |
| 配置与密钥灾备 | `backup-all.sh` verify/rebuild-drill | 加密介质保管和完整业务恢复 |
| 网络离线 | Compose/包可静态核对 | 专用 daemon、出口规则和 daemon 访问审计 |
| 宿主机重启 | 无 | 本轮明确 `NOT RUN` |
| 长期稳定性 | 无 | 需要另设观察窗口，不能由短期验收替代 |

## 17. 完成签署

只有以下条件同时满足才能签署“本轮验收通过”：

- 所有纳入范围的自动门禁无 `FAIL`；警告均有处置结论。
- 全部自研 `linux/amd64` 镜像真实构建，交付包在独立目录校验和导入成功。
- 平台/前端独立运行，每个启用子系统完成受控接入和对应真实业务验收。
- 故障、升级、回滚和恢复演练的范围、数据和结果均有证据。
- 所有 `BLOCKED`、`NOT RUN`、RTO/RPO 和保留资源已单列，未被包装成通过。
- 手册命令与最终交付包逐项对照，至少从空部署目录完整执行一次。

签署：

```text
执行人：                日期：
业务复核人：            日期：
运维复核人：            日期：
最终结论：PASS / FAIL / CONDITIONAL PASS
条件和到期日：
```
