# 统一交付检查与安装恢复

交付检查、服务器预检和正式安装共用 `服务器一键离线部署.sh`。完整新版工具随新交付介质提供；原交付包及其校验文件应保留，不要替换包内脚本后重写原 SHA256SUMS。

> 源码核对：2026-10-22。本页描述通用脚本能力，不是某批介质或服务器的验收记录。固定签名批次步骤见[操作说明索引](../../../操作说明/README.md)。

## 统一只读计划（2026-10-25）

已安装环境新增统一计划入口，根 `--check-only`、`bin/deploy.sh`、运维工具和 CI 使用同一检查核心：

```bash
cd /opt/unified-identity-platform
./bin/deploy.sh plan --action upgrade --component platform --format human
./bin/deploy.sh plan --action upgrade --component platform --format json
# 只有已选择前向恢复并持有受控材料时：
./bin/deploy.sh plan --action upgrade --component platform --forward-recovery \
  --recovery-evidence /secure/recovery-evidence.json --format human
# 执行前可用此前报告检测现场变化：
./bin/deploy.sh plan --action upgrade --component platform --format human \
  --baseline-report /secure/plan-before.json
# 根入口可在 check-only 保存基线，正式升级时消费 .env 指纹：
sudo bash ./服务器一键离线部署.sh --media <介质> --target <目录> --install --check-only \
  --upgrade --plan-baseline /secure/plan-baseline.json
sudo bash ./服务器一键离线部署.sh --media <介质> --target <目录> --install \
  --upgrade --plan-baseline /secure/plan-baseline.json
```

平台发布前还会执行只读 `./api config-check`（复用 API `config.Load`，不启动 HTTP/worker、不生成密钥），配置无效时在数据库迁移前失败。

退出码：`0` 可执行，`2` 有阻断，`3` 需要外部输入或人工处理，`64` 参数错误。计划只读，不启动/停止容器，不修改配置、会话、发布指针或数据库；执行入口仍会在部署锁内重新核对镜像、配置、数据库和目标状态，旧计划不能作为授权来源。

报告至少包含：操作、目标、组件、实际执行账号、当前/目标镜像摘要、配置/编排/兼容性指纹、稳定检查编号、状态、严重程度、脱敏证据、修复建议、预计阶段、服务影响和恢复边界。权限检查以调用账号为准；root 的 `--check-only` 不代替部署账号的权限验证，正式执行前应以部署账号复跑 `plan`。检查覆盖介质/镜像、资产事务、控制面重载标记、安装会话、必填配置/重复键与租户、实际账号可读写的备份/文件网关路径、磁盘/inode、Docker/Compose、数据库连接，以及升级/恢复兼容性。

新发行应携带 `deployment-compatibility.json`（`uip.deployment-compatibility/v1`），由 `bin/build-deployment-compatibility.py` 在资产构建时生成，表达资产/配置契约、迁移目标、迁移集合哈希、上一程序迁移后兼容性和恢复类型。兼容性证据必须来自独立的 `uip.deployment-compatibility-evidence/v1` 文件且迁移哈希匹配；否则保持 `unknown`。恢复兼容性未知或明确不兼容时，普通升级阻断；只有显式选择前向恢复并提交完整备份摘要、迁移目标和经验证步骤，才允许继续。根升级在资产安装后、迁移前再次执行 `--compatibility-only` 门禁；不满足条件时停止，不能凭旧计划继续。历史介质不修改、不重签、不补写原 manifest。

`--check-only` 在已安装目标上复用该计划；阻断时返回非零，待办时保留介质检查结论并提示正式执行前处理。CI 已纳入计划单测和兼容性声明资产，并提供仓库变量 `ENFORCE_DEPLOY_COMPATIBILITY=true` 的远端兼容性门禁；默认关闭，待当前批次兼容证据齐备后启用。新工具和声明必须同步进入资产白名单、打包内容、安装校验和测试夹具。

## 场景化打包入口（2026-10-25）

根打包器保留全部旧参数，并新增只读/门禁入口：

```bash
./一键制作离线镜像包.sh plan --scenario fresh --spec <release-spec.json> --components contract,project
./一键制作离线镜像包.sh plan --scenario update --spec <release-spec.json> --write-plan /绝对路径/release-plan.json
./一键制作离线镜像包.sh build --version <版本> --components contract,project --release-integrity-spec <release-spec.json>
./一键制作离线镜像包.sh status --output <交付目录>
./一键制作离线镜像包.sh package --output <已冻结目录> --version <版本> \
  --verifier /usr/local/bin/release-verifier \
  --trust-dir /etc/unified-identity-platform/release-trust
```

- `plan` 只读计算应用、登记协议、必需材料和下一步，不构建、不签名；
- 材料计划通过 `release-materials.py --output` 原子冻结为 0600 JSON；
- `build/resume` 在调用旧构建入口前执行协议门禁：V1 缺少正式运行批准阻断，V2 不要求逐镜像批准；
- `finalize` 校验绝对路径、SHA256SUMS、密钥/信任/verifier 和重复签名后，才调用 `finalize-release-integrity.sh`；
- `package` 只打包已冻结目录，默认要求已签名 `release-manifest.json`；旧介质需显式 `--allow-legacy-unsigned`；
- `release-plan.json` 为 `BLOCKED`、schema 错误或未知状态时，服务器入口拒绝继续安装。

## 先明确发布信任与登记模式

`--check-delivery` 只做结构与声明检查，不验正式发行签名，也不证明实例商业授权或当前ACK。正式签名部署应先通过独立可信渠道安装 verifier/公钥，再用**已解压目录**启用以下门禁；总TAR应先核对传输摘要并安全解压：

```bash
sudo bash ./服务器一键离线部署.sh \
  --media /data/uip-delivery/本批已解压目录 \
  --target /data/unified-identity-platform --install --check-only \
  --require-release-integrity \
  --release-verifier /usr/local/bin/release-verifier \
  --release-trust-dir /etc/unified-identity-platform/release-trust
```

verifier和trust目录必须在待验介质之外；验签在执行任何介质脚本、依赖安装和目标变更前完成。正式安装保留相同验签参数并去掉 `--check-only`；恢复也保留原参数。该门禁不适用于 `--check-delivery`/`--status`，不能把它们合并成一次正式验签。

当前平台组件登记支持内置目录 V2 及 V1 桥接；商业 JWS 和签名快照仍为协议 1。V2 普通镜像更新保持组件身份；遗留 V1 批准发布走独立审核/发布器。交付结构检查器会读取 `release-manifest.json` 与配套证明，按应用的 `registration_protocols` 分别检查：V2 用当前目录的完整组件、实际镜像及六份生产 profile 绑定，不额外要求目标 V1 批准和目标 evidence；V1 应用继续校验对应原材料。合法的跨应用 V1/V2 发行可以共存，同一应用内矛盾、未知版本或缺组件则拒绝。

有发行证明时，检查器还会核对全部 artifact 大小/摘要、总 SHA 与原始/normalized/资产 profile 字节；目录和总 TAR 均支持白名单内 `profiles/`、`attestations/`，不执行介质代码。Statement 与 bundle 须为合法严格 JSON，并核对现行 Go 协议中的类型、subject、签名材料编码及原始载荷/消息摘要绑定；破损封装不能显示结构 READY。没有发行清单的旧介质保留原材料规则，不完整或损坏证明不能被忽略降级。`authority.release-signature` 仍为待受信 verifier 验证：本入口不验证签名真实性、构建者身份或信任策略，不建立发行信任；显式安装计划的独立旧安装基线、实例许可证、迁移资格和自然 ACK 也各自保留。详见[授权总览](../../../sharedDocs/09_商业授权/README.md)。

## 在打包电脑检查交付

```bash
bash ./服务器一键离线部署.sh \
  --media /path/uip-offline-delivery-批次-linux-amd64.tar \
  --check-delivery
```

`--media` 同时支持总 TAR（配套 `.sha256`）和已解压目录。此入口使用 Python 标准库读取归档，不执行介质脚本、不调用 Docker、不需要 root、不解压到部署目录。JSON 输出加 `--format json`。

检查报告会列出镜像真实内容与架构/版本、Platform/前端/公共包/资产兼容锁、生产组件清单、运行批准、安装场景、目标证据和许可证声明，并告诉操作者下一步在厂商电脑、服务器或基础平台执行。

| 状态 | 含义 |
| --- | --- |
| `READY` | 静态交付材料结构配套；发布签名仍由介质外受信 verifier 验证，真实实例、商业授权及当前运行 ACK 仍由平台验证。 |
| `NEEDS_INPUT` | 存在明确待办。例如候选交付尚未签发正式授权，允许先安装基础平台取得真实实例申请。 |
| `INVALID` | 介质损坏、版本/镜像/组件不一致、未知字段或不安全归档；应恢复原介质或重新制作。 |

检查命令退出码：`0` 表示介质可进入准备流程（可能仍有授权待办），`2` 表示结构或完整性错误。安装阶段对已声明安装计划/授权交付缺少必需批准或证据返回 `3`。后签许可证本身不会阻断合法的基础平台安装。不能用伪造签名或修改摘要绕过平台验证。

## 在服务器预检和安装

```bash
bash ./服务器一键离线部署.sh \
  --media /data/uip-delivery/本次总包.tar \
  --target /data/unified-identity-platform --install --check-only

sudo bash ./服务器一键离线部署.sh \
  --media /data/uip-delivery/本次总包.tar \
  --target /data/unified-identity-platform --install
```

`--check-only` 另外检查服务器架构、Docker/Compose、Docker 和 containerd 所在分区的容量、宿主命令及隔离资产安装条件；不会创建安装会话或启动容器。部署路径不代替 Docker/containerd 数据根目录迁移。

安装过程按模式生成固定阶段：宿主依赖、完整备份（升级）、部署资产、编排合并（升级）、镜像包、配置、各组件镜像导入、平台发布、前端发布、逐系统候选准备/受控暂存、授权处理和验收。每阶段显示进度；候选准备完毕会明确显示待平台采用，不能当作业务已经运行。

进度位于 `/data/unified-identity-platform.installation-state`，目录权限 `0700`、JSON `0600`。独立安装执行锁位于同级 `.installation-run.lock`，不占用平台 Agent 必需的 `runtime/.deploy.lock`。日志只保存受限状态、摘要、路径和固定原因码，不保存运行密码、JWS 或私钥内容。

## 中断后查看与继续

```bash
sudo bash ./服务器一键离线部署.sh \
  --target /data/unified-identity-platform --status

sudo bash ./服务器一键离线部署.sh \
  --media /data/uip-delivery/本次总包.tar \
  --target /data/unified-identity-platform --install --resume
```

恢复时复用原介质、目标、模式、组件和选项（例如原 `--skip-host-deps`、`--apply-compose`）；路径、总清单摘要或外部 `--license` 内容变化都会拒绝沿用旧会话。`--status` 不需要介质、不调用 Docker，只展示日志事实，不能证明此刻容器健康。

恢复会重新检查实际文件、包内容、精确导入记录、本机镜像与本地 Registry 对象，以及已发布服务的目标镜像、健康和完整 Worker 集合。已完成的发布只有当前事实仍吻合时才保留。历史发布证据失效会停止并要求先修复；不会按一行 `DONE` 自动覆盖外部变更。未完成发布仍走原备份、数据库迁移校验和受控恢复门禁。

已有安全配置会保留 IP、密钥、管理员设置和实例身份；配置残缺时会指出需要修复的阶段，不重新生成凭据。升级恢复保守地重新做完整备份。未完成资产事务必须先使用同包 `install-assets.sh --recover <目标目录>` 恢复原资产，再继续；不会删除事务标记制造成功，也不会自动回滚数据库。

## 等待平台或授权

`WAITING` 是可继续的人工待办。根据报告在基础平台对对应 `prod` 应用完成采用/受控更新，或在厂商电脑准备与真实实例、发行镜像一致的批准和许可证。候选处于 `READY_TO_CONTINUE` 时按原工具明确执行 `continue`；一键恢复不会自动删除候选或替代用户进行采用。

```bash
cd /data/unified-identity-platform
sudo bash ./bin/deploy.sh status contract
# 只有状态确为 READY_TO_CONTINUE 才执行：
sudo bash ./bin/deploy.sh continue contract
sudo bash ./bin/deploy.sh verify contract
```

人工待办处理完成后，再用原一键命令追加 `--resume`。 首次未带外部 `--license` 的会话，可在基础平台独立导入/续期，恢复时仍保留原选项；已完成的授权处理不会重新导入旧交付 JWS。只有本次启用系统的实际运行、候选收尾、平台清单以及整个当前购买集合的有效签名授权、当前快照和自然 ACK 均通过时，才记录完整验收。`license-install verify-enforcement` 仅观察当前授权执行状态，可更新正常授权时间高水位，不导入许可证、不发起新的授权激活或代发 ACK；保留正常时间观察及预定生效转换。

## 后续批次

旧会话完整验收后，可显式使用 `--new-session` 开始后续升级或增装。旧会话会安全归档到同级 `.installation-history/<session-id>.json`；原数据库、实例、授权和备份保留。

```bash
sudo bash ./服务器一键离线部署.sh \
  --media /data/uip-delivery/下一批总包.tar \
  --target /data/unified-identity-platform \
  --upgrade --apply-compose --new-session
```

`--new-session` 不能与 `--resume` 同用，也不能跳过未完成、失败或等待人工的旧会话。编排有差异时必须显式 `--apply-compose` 或交互批准；`--yes` 只确认资源警告。

### 打包前门禁补充（2026-10-26）

正式 `package` 必须提供介质之外的独立 verifier 和固定信任目录，验签返回 VERIFIED 后才创建总归档；签名文件存在本身不算通过。旧的无签名介质仅保留显式 `--allow-legacy-unsigned` 兼容入口，不用于新发行。

材料检查程序异常、输出无效或返回 BLOCKED 时拒绝构建。计划基线必须来自 READY 报告；完整计划比较组件范围及现场指纹，资产替换后的兼容性检查同时复核运行配置和兼容性声明指纹，不能凭旧声明继续。

显式使用 `--plan-baseline` 的首次升级会在安装运行锁内、会话创建前再次核验完整计划，防止预检与人工确认期间发生的现场变化被遗漏。续跑身份绑定同一路径和原计划摘要，不能替换计划后复用旧会话。安装执行锁只协调同一安装入口；独立运维操作仍需遵循各自发布锁和受控窗口。

## 构建前正式发布检查（2026-10-27）

根目录 `一键制作离线镜像包.sh` 默认按正式发布检查输入，`--preflight-only` 也执行相同检查。它在任何镜像 build/save 前检查所选业务、平台、前端的外部 FROM（含多阶段和 ARG），错误包含 Dockerfile 路径与行号。不自动选择或改写摘要。

先配置真实构建身份 `UIP_BUILD_BUILDER_ID`，需与后续信任策略匹配。命令行未设置 `UIP_BUILD_INVOCATION_ID` 时自动生成本次唯一 UUID，不生成或猜测构建身份；Web 生成的作业脚本已设置这两个变量。不要复用旧 invocation 或冒用其他构建者身份。正式签名原本就要求这些证据字段，本次把失败提前到构建前。

完整构建先只读检查公共基础镜像的目标平台和 RepoDigest；冷缓存或本地仅有其他架构时，自动通过仓库清单只读确认 linux/amd64 与摘要，不要求人工逐个拉取。正常构建随后沿用原流程自动准备/拉取全部公共镜像，并在第一条 build/save 前核验实际来源；`--preflight-only` 只解析元数据，不执行 pull/build/save。仓库无法解析或没有目标架构时提前报错。七项公共镜像证据不能代替业务 Dockerfile 检查。构建后的实际镜像身份、来源证据、许可证和正式签名门禁仍必须通过；预检通过不是交付验收。

只制作开发/审核用未签候选时，显式加 `--candidate`（可与 `--preflight-only` 配合）；此模式允许尚不满足正式签名条件的输入，不允许同时传正式签名密钥，不绕过原有许可证、来源或完整性检查。底层 `build-offline-packages.sh` 保留候选构建用途，正式日常交付使用根入口。

`--extend-from` 共享指纹不匹配时，使用基线 SHA256SUMS 绑定的 `source-input-index.tsv` 列出共享文件变化；旧包没有有效索引时只报告无法定位，不猜测变化来源。合同业务 Dockerfile 不属于 shared 指纹范围。历史 r1 材料缺失，不能据此还原当时哪些共享文件发生变化。

本次不增加镜像缓存或断点续跑参数。若构建已成功而签名/总归档未完成，应先核验冻结介质再使用现有 `finalize` / `package`；不要把修改当前 Dockerfile 后补证据当作旧镜像的真实构建证明。

公共镜像平台检查兼容 OrbStack 的描述符布局：顶层 Os/Architecture 为空时，只接受有效单镜像 Descriptor.platform 的 linux/amd64；不把“未知平台”当成功。仓库索引按 linux/amd64 描述符检查，单平台清单按固定 digest 读取 Image 配置确认；实现参考 Docker 官方 imagetools inspect 的 Manifest/Image JSON 输出（https://docs.docker.com/reference/cli/docker/buildx/imagetools/inspect/）。
