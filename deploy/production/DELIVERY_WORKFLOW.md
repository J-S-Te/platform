# 统一交付检查与安装恢复

交付检查、服务器预检和正式安装共用 `服务器一键离线部署.sh`。完整新版工具随新交付介质提供；原交付包及其校验文件应保留，不要替换包内脚本后重写原 SHA256SUMS。

> 源码核对：2026-10-22。本页描述通用脚本能力，不是某批介质或服务器的验收记录。固定签名批次步骤见[操作说明索引](../../../操作说明/README.md)。

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
