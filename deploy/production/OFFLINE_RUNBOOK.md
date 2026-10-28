# 统一身份认证平台：构建、离线部署与恢复操作手册

> 源码核对：2026-10-22（静态文档更新，非新部署验收）
> 适用版本：当前工作区 `platform/deploy/production/` 交付链路
> 文档地位：**这是本目录构建和离线部署的详细操作手册。** 根目录 `操作说明/` 提供固定发行批次示例，`DELIVERY_WORKFLOW.md` 说明统一入口与续跑。`README.md` 与
> `OFFLINE_DEPLOYMENT.md` 只提供入口和摘要；备份实现细节见
> [BACKUP_RECOVERY.md](BACKUP_RECOVERY.md)。脚本帮助与本手册不一致时，先停止操作并按缺陷处理，不能自行跳过门禁。

本手册默认两台机器：一台联网构建机、一台离线部署机。附录 A 给出同一台空白测试机先构建、再隔离验收的边界。示例中的 `<发布版本>`、`<交付目录>`、`<部署机地址>` 必须替换；不要原样输入尖括号占位符。

## 1. 环境与角色

### 1.1 角色、目录和权限

| 角色 | 建议系统 | 工作目录 | 执行身份 | 网络 |
| --- | --- | --- | --- | --- |
| 联网构建机 | Linux x86_64；arm64 仅作为较慢的交叉构建备选 | 完整源码根目录 | 可使用 Docker 的构建用户 | 可访问源码依赖与公共镜像仓库 |
| 离线部署机 | Linux x86_64 | `/opt/unified-identity-platform` | 安装、配置和恢复使用 `root`；日常只读检查可用获授权运维用户 | 允许内网访问，不要求公网 |
| 交付中转介质 | 受控文件服务器或加密移动介质 | 独立发布目录 | 只允许发布人员写入 | 不得混入运行配置或备份 |

本文示例部署根为 `/opt/unified-identity-platform`，并非固定磁盘限制。中文入口用 `--target`、底层入口用各自 `--dir`/部署根参数选择同一实际目录，例如 `/data/unified-identity-platform`；Docker/containerd存储根独立配置。脚本不会保存 SSH 密码；密码只允许在 SSH 客户端交互提示中输入，禁止写进命令、脚本、手册、聊天记录或日志。

正式签名批次先按 [DELIVERY_WORKFLOW.md](DELIVERY_WORKFLOW.md) 设置介质外可信 verifier/trust，并对解压目录启用 `--require-release-integrity`。结构检查与SHA不能替代发行验签。当前登记支持V2/桥接；本手册涉及镜像运行批准的章节仅用于遗留V1或显式采用该材料的安装路径，不能解释成每次V2重建都需重新批准身份。

### 1.2 资源基线和容量估算

以下是首次验收的建议起点，不是业务容量上限：

- CPU：至少 8 核；同时构建全部镜像建议 12 核以上。
- 内存：部署至少 16 GiB，完整构建建议 24 GiB 以上。
- 磁盘：构建机建议至少 80 GiB 可用；部署机建议 Docker 数据目录至少 60 GiB，部署/备份分区至少 40 GiB，并为业务数据库、文件和长期备份另行扩容。
- inode：Docker 数据目录与部署分区分别建议至少 100,000 个可用 inode。
- CPU 架构：交付目标固定为 `linux/amd64`，服务器上的 `uname -m` 应为 `x86_64`。

**16 GiB是完整部署建议起点，当前中文脚本在低于该值时请求资源警告确认，并非不可跳过的代码硬门禁。** 获得继续提示不证明资源足够；完整平台或包含Metabase时应先按实际服务预算评估和扩容。增加swap、无限延长健康等待或删除探针不能替代容量评估。Metabase首次初始化同时运行JVM、数据库与平台依赖，内存不足可能导致换页、Docker API/SSH停顿。分阶段观察 `free -h`、容器健康与宿主响应，出现持续换页或swap耗尽时停止新增模块。

### 1.3 两台机器先做只读预检

**执行卡：构建机；任意目录；构建用户；只读。**

```bash
uname -a
uname -m
getconf _NPROCESSORS_ONLN
free -h
df -hT
df -ih
docker version
docker buildx version
docker compose version
bash --version | head -n 1
```

预期：架构为 `x86_64` 或明确知道正在使用 arm64 交叉构建；Docker、Buildx、Compose 可用；磁盘和 inode 满足预算。失败时先补资源或工具，不要进入构建。

**执行卡：离线部署机；`/root`；root；只读。**

```bash
uname -a
uname -m
free -h
df -hT
df -ih
date -Is
timedatectl status || true
docker version 2>/dev/null || true
docker compose version 2>/dev/null || true
docker ps -a 2>/dev/null || true
docker volume ls 2>/dev/null || true
ss -lntup
ip -brief address
ip route
```

预期：保存一份不含密码、Token、环境变量正文的预检记录。若发现已有 `uip-*` 容器、`uip` Compose 项目、同名卷、`/opt/unified-identity-platform` 数据或计划端口已占用，这台机器不再视为空白机；保留现场并先评审，禁止直接删除。

## 2. 空白构建机准备

### 2.1 安装系统工具

下面以 Ubuntu 22.04/24.04 为例。Docker 软件源应使用组织批准的软件源；如果系统已安装符合要求的 Docker，不重复安装或切换数据根。

**执行卡：联网构建机；任意目录；root。**

```bash
apt-get update
apt-get install -y bash ca-certificates curl git gzip jq tar coreutils findutils openssl
```

安装 Docker Engine、Buildx 和 Compose plugin 后验证：

```bash
docker info
docker buildx version
docker compose version
bash -c '((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3)))'
```

预期：三条检查均返回 0。若 Docker daemon 不可达，先检查服务与用户权限；不要用 `chmod 666 /var/run/docker.sock` 绕过权限。

### 2.2 准备完整源码和构建输入

**执行卡：联网构建机；源码父目录；构建用户。**

源码根至少包含：

```text
platform/
frontend/
customer_and_opportunity/
contract_management/
project_management/
Settlement/
data_analysis/
```

进入源码根并确认脚本入口：

```bash
cd <源码根目录>
test -x platform/deploy/production/bin/build-offline-packages.sh
test -f platform/deploy/production/docker-compose.yml
find . -maxdepth 2 -name Dockerfile -type f -print | sort
```

预期：没有缺失子工程。构建脚本只能从完整源码树执行；交付包中的同名脚本是审计资产，不能脱离源码构建业务镜像。

### 2.3 记录源码与工具链指纹

**执行卡：联网构建机；源码根；构建用户；只读。**

若有 Git 元数据：

```bash
git status --short
git rev-parse HEAD
git submodule status --recursive 2>/dev/null || true
```

若交付源码不含 Git 元数据，记录正式打包器生成的 BUILD_INFO.txt 内 SOURCE_INPUT_SHA256，
并保存经审核的源版本标识。不要对整个工作区递归哈希：实际 .env、私钥、运行数据和日志可能混入清单。
该指纹只证明打包输入身份，不是源码或 Secret 的备份。

同时保存以下非敏感版本输出：

```bash
docker version > /tmp/uip-docker-version.txt
docker buildx version > /tmp/uip-buildx-version.txt
docker compose version > /tmp/uip-compose-version.txt
```

预期：验收报告记录源码指纹和工具版本，但不把 `.env`、私钥、凭据或数据库文件纳入公开报告。

### 2.4 网络和代理

构建 Dockerfile 会下载 Go、Node、Alpine 包等依赖。代理必须通过构建环境或受控镜像源提供，不要把凭据写进 Dockerfile。公共基础设施还需要 MySQL、Keycloak、Temporal、Metabase、Prometheus、Node Exporter、Registry 和 Docker Socket Proxy 镜像。

可先执行一次完整预拉；失败时修复网络后重复，不需要删除已经下载成功的层：

```bash
docker pull --platform linux/amd64 mysql:8.4
docker pull --platform linux/amd64 quay.io/keycloak/keycloak:26.2
docker pull --platform linux/amd64 temporalio/auto-setup:1.29.7
docker pull --platform linux/amd64 metabase/metabase:v0.53.7
docker pull --platform linux/amd64 prom/prometheus:v3.5.0
docker pull --platform linux/amd64 prom/node-exporter:v1.9.1
docker pull --platform linux/amd64 registry:2.8.3
docker pull --platform linux/amd64 ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459
```

预期：每个镜像可通过 `docker image inspect` 读取。CloudFront、Go Proxy 或 Alpine 镜像源超时属于构建网络问题，不要通过删除 `go.sum`、关闭校验或改成未审核依赖解决。

如组织提供受控依赖镜像，可在构建命令前设置以下非敏感地址；脚本只把它们作为 Docker build argument 传给已声明对应 `ARG` 的 Dockerfile，不写入离线包元数据：

```bash
export OFFLINE_GOPROXY='https://<组织Go代理>|https://proxy.golang.org|direct'
export OFFLINE_GOSUMDB='sum.golang.google.cn'
export OFFLINE_NPM_CONFIG_REGISTRY='https://<组织npm代理>'
export OFFLINE_PULL_RETRIES=3
export OFFLINE_PULL_RETRY_DELAY_SECONDS=5
```

代理需要认证时使用构建平台的 Secret/凭据机制，不把用户名、密码或 Token 写进上述环境变量、Dockerfile 或操作记录。可以替换受控 SumDB 地址，但 `OFFLINE_GOSUMDB=off` 会被明确拒绝；私有模块以后应使用单独评审的 `GOPRIVATE` 方案，不能关闭全局模块校验。公共镜像拉取采用有界重试；到达上限仍失败就停止，不无限等待。

## 3. 构建打包

### 3.1 发布版本和输出目录

一次交付只使用一个唯一版本，例如：

```bash
export UIP_RELEASE_VERSION=20260927-acceptance01
export UIP_DELIVERY_DIR=/srv/uip-delivery/$UIP_RELEASE_VERSION
install -d -m 750 "$UIP_DELIVERY_DIR"
```

版本只能含字母、数字、点、下划线和连字符。已发布版本不可变；重建内容不同时必须换新版本，不能覆盖旧包。

### 3.2 构建全部自研镜像与资产

**执行卡：联网构建机；源码根；可使用 Docker 的构建用户。**

```bash
cd <源码根目录>
time platform/deploy/production/bin/build-offline-packages.sh \
  --component all \
  --version "$UIP_RELEASE_VERSION" \
  --output "$UIP_DELIVERY_DIR"
```

预期：退出码为 0；输出目录包含 9 个镜像包、1 个与构建机 Ubuntu
`VERSION_ID`/代号严格绑定的宿主依赖包、1 个部署资产包、每包独立
`.sha256`、包外独立安装入口 `install-host-dependencies.sh` 和
`install-assets.sh`（均含 sidecar）、`BUILD_INFO.txt` 和总 `SHA256SUMS`。
`data-analysis-backend` 包内含 4 个运行镜像。宿主依赖包包含
`jq/curl/ca-certificates/tar/gzip/coreutils` 及部署、备份和恢复路径需要的
其他系统命令依赖，但不包含 Docker Engine/Compose。完整耗时取决于网络与缓存，
验收报告只记录实测时间，不写固定保证。

组件和命令参数对应关系：

| `--component` | 产物前缀 | 内容 |
| --- | --- | --- |
| `common` | `common-infrastructure` | 8 个第三方镜像和 File Gateway 自研镜像 |
| `platform` | `platform-backend` | 平台 API、Worker、迁移和部署 Agent |
| `frontend` | `frontend` | 统一前端/Nginx 网关 |
| `customer-opportunity` | `customer-opportunity-backend` | CRM API 与 Workers |
| `customer-portal` | `customer-portal-backend` | 客户门户 API 与 Worker |
| `contract` | `contract-backend` | 合同系统 |
| `project` | `project-backend` | 项目系统 API 与 SLA Worker |
| `settlement` | `settlement-backend` | 结算 API 与 Worker |
| `data-analysis` | `data-analysis-backend` | API、聚合、告警、迁移镜像 |
| `host-deps` | `host-dependencies-ubuntu-*` | Ubuntu/amd64 本地 APT 仓库与完整依赖闭包 |
| `assets` | `deployment-assets` | 安装器、Compose、脚本、清单、模板和本手册 |

只重建单组件时使用同一唯一版本和同一输出目录，例如：

```bash
platform/deploy/production/bin/build-offline-packages.sh \
  --component frontend --version "$UIP_RELEASE_VERSION" --output "$UIP_DELIVERY_DIR"
```

#### 基于已部署基线增量制作业务子系统

目标机已完成首装、本次只追加业务子系统时，使用根目录一键入口的 `--extend-from`，不直接把一个新业务包与任意基础包手工拼接：

```bash
cd <源码根目录>
BASE_DELIVERY="<目标机首装时的原交付目录>"
UIP_RELEASE_VERSION="$(date -u +%Y%m%dT%H%M%SZ)-add-project"

./一键制作离线镜像包.sh \
  --version "$UIP_RELEASE_VERSION" \
  --extend-from "$BASE_DELIVERY" \
  --components project \
  --no-pause
```

该模式必须显式指定 `--components`，至少一个业务系统。它先校验基线总清单和 sidecar，只构建所选业务镜像，再原样复用 common/platform/frontend/deployment-assets 及可用的宿主依赖。产物的 `BASE_DELIVERY_INFO.txt` 记录基线 `SHA256SUMS` 摘要，服务器使用第 7 章 `--add-subsystems` 处理。

`--extend-from` 不是跨版本兼容自动判定器。若所选子系统需要新 Compose 服务、新清单/模板字段、平台 API 协议、前端路由或共享基础设施变更，必须建立新完整基线并走受控升级；禁止使用不同服务器/不同版本的交付目录作基线。

### 3.3 构建失败、磁盘不足和重试

- 所有正式包先写同一输出目录内的隐藏临时文件，完成结构与 SHA 校验后原子提交。
- 同名正式包内容不同会被拒绝；失败构建不能破坏已有可用产物。
- 空间不足时清理本次构建明确产生的临时内容或扩容，不执行全局 `docker system prune -a`。
- 重试同版本仅允许复用字节完全一致的产物；源码、依赖或构建参数改变后使用新版本。
- 看到部分组件成功，不代表 `all` 已完成；必须检查退出码、清单和所有预期包。

### 3.4 校验包结构、架构和摘要语义

**执行卡：联网构建机；交付目录；构建用户；只读。**

```bash
cd "$UIP_DELIVERY_DIR"
sha256sum --check SHA256SUMS
for sidecar in *.tar.gz.sha256; do sha256sum --check "$sidecar"; done
sha256sum --check install-host-dependencies.sh.sha256
sha256sum --check install-assets.sh.sha256
cat BUILD_INFO.txt
for pkg in *-linux-amd64.tar.gz; do
  printf '\n===== %s =====\n' "$pkg"
  tar -xOf "$pkg" package.env
done
```

每个镜像包的 `package.env` 必须显示：

- `PACKAGE_FORMAT=2`；
- 正确 `COMPONENT` 与本次 `VERSION`；
- `PLATFORM=linux/amd64`；
- `IMAGES` 数量与组件相符；
- `IMAGE_CONFIG_DIGESTS` 为每个保存镜像的配置对象摘要。

摘要不可混用：

- 包的 `.sha256`：整个 `tar.gz` 文件摘要；
- 包内 `SHA256SUMS`：`images.tar` 摘要；
- `IMAGE_CONFIG_DIGESTS`：Docker image config 摘要，用于核对导入对象；
- OCI image index/manifest 摘要：归档内部对象摘要；
- 部署使用的 `image@sha256:...`：镜像推入本机 Registry 后得到的不可变 Registry manifest digest，由 `prepare/deploy` 写入 `.release.env`。

目标 Docker 使用 containerd 镜像存储时，`docker image inspect .Id` 可能报告
image index/manifest 摘要而不是选定平台的 config digest。导入器先使用 `.Id`
核对配置摘要；若不同，优先从已通过 SHA256 校验的原 `images.tar` 一次性读取
小型 OCI 元数据，核验精确标签→索引/清单→唯一 `linux/amd64` 配置的关联、
每个关联元数据对象的 SHA256/size 及清单指定的 config digest。Docker 返回的
ID 恰好是这条已核验链的索引/清单摘要时，直接显示“镜像校验通过”，无需再次
导出所有文件系统层。不会拿任意 RepoDigest、其他标签或其他平台的摘要放行。
已识别的 OCI 元数据损坏、摘要/大小不一致或平台选择歧义会直接拒绝；
旧归档、不支持的布局或缺少身份关联时，仍临时重新导出已加载的精确标签，并
核对 `linux/amd64` config blob；等待期间打印明确进度。真实配置摘要不同仍拒绝。
临时验证标签和归档在成功或失败后都会清理，所有包/架构/发布门禁保持有效。
镜像包解压和重新导出的临时文件默认写入部署目录的
`runtime/import-tmp/`，不使用可能只有少量容量的 `/tmp` tmpfs。安装器的部署分区
空间预算已覆盖该路径。如需使用独立大容量分区，执行导入或安装前显式设置
`UIP_IMPORT_TMPDIR=/绝对路径`；该路径不得是符号链接，并会被收紧为 `0700`。

生产 Agent 必须读取宿主机严格为 `0600` 的 `.env/.release.env` 并原子维护
`runtime/*.env`。Docker 对非 root `user` 不会把 Compose `cap_add` 变成有效能力，
因此编排以 `0:10001` 启动 Agent，同时 `cap_drop: [ALL]` 后只恢复
`CHOWN/DAC_OVERRIDE/FOWNER`，并继续启用只读根文件系统、
`no-new-privileges` 和 Docker API 白名单代理。不要把它改回无有效能力的
`10001:10001`，也不要为了绕过权限错误放宽凭据文件到 `0644`。Agent 创建的
Unix Socket 使用共享 GID 10001 和 `0660`，平台 API 仍以非 root 身份访问。

`ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459` 内置 HAProxy 3.4，日志级别必须使用
`warning`，不能使用会导致配置解析失败和重启循环的缩写 `warn`。编排会等待代理
健康后才启动 Agent。首次安装失败时，若上一发布配置仍是 `:main/:pending` 等占位
镜像，脚本只恢复配置文件并明确跳过运行态回滚，不会在离线服务器拉取公网占位镜像。

不能把 image config、manifest、index 或包摘要互相替代。当前导入器兼容 Docker classic `docker save` 和 OCI 布局；最终是否可导入仍以目标 Docker 的真实 `docker load` 和导入校验为准。

需要诊断归档布局时只读检查一个包，不修改 `package.env`：

```bash
work_dir="$(mktemp -d /tmp/uip-image-layout.XXXXXX)"
tar -xOf <镜像包.tar.gz> images.tar > "$work_dir/images.tar"
tar -tf "$work_dir/images.tar" | grep -E '(^|/)(manifest\.json|index\.json|oci-layout)$'
rm -rf "$work_dir"
```

看到 `manifest.json` 表示 Docker classic 归档；看到 `index.json` 与 `oci-layout` 表示 OCI 归档。两者都是支持路径。若目标服务器 `deploy.sh import` 的真实 `docker load` 失败，记录 Docker Engine/存储后端版本和完整错误，在相同 Linux/AMD64 Engine 上重建或升级目标 Engine；不要手工转换摘要、删除 blob 或降低导入校验。

`BUILD_INFO.txt` 记录发布版本、目标平台、审核构建输入的聚合 SHA-256 以及 Docker/Buildx 版本；它不列出文件内容、密码或代理凭据。构建报告应保存该文件，若源码缺少 Git 元数据，就以 `SOURCE_INPUT_SHA256` 作为本次源码/部署资产输入指纹。

### 3.5 检查资产包无运行数据

```bash
tar -tzf "deployment-assets-${UIP_RELEASE_VERSION}.tar.gz" | LC_ALL=C sort
if tar -tzf "deployment-assets-${UIP_RELEASE_VERSION}.tar.gz" \
  | grep -E '(^|/)(\.env$|\.release\.env$|.*\.bak$|.*\.pem$|.*\.key$|dump[^/]*\.sql(\.gz)?$)'; then
  echo '错误：资产包含疑似运行 Secret 或数据' >&2
  exit 1
fi
```

预期：资产包包含 `bin/install-assets.sh` 和 `OFFLINE_RUNBOOK.md`，但不含运行 `.env`、私钥、备份、数据库或业务文件。命中时隔离整批交付物并修复白名单，不能手工删文件后继续发布。

## 4. 复制与校验交付

### 4.1 必须传输的完整内容

传输以下文件，不传 Docker 缓存或源码配置：

```text
SHA256SUMS
BUILD_INFO.txt
install-assets.sh
install-assets.sh.sha256
install-host-dependencies.sh
install-host-dependencies.sh.sha256
host-dependencies-ubuntu-<VERSION_ID>-<代号>-<版本>-linux-amd64.tar.gz
host-dependencies-ubuntu-<VERSION_ID>-<代号>-<版本>-linux-amd64.tar.gz.sha256
deployment-assets-<版本>.tar.gz
deployment-assets-<版本>.tar.gz.sha256
common-infrastructure-linux-amd64.tar.gz
common-infrastructure-linux-amd64.tar.gz.sha256
其余 8 个 *-<版本>-linux-amd64.tar.gz
每个镜像包对应的 .sha256
```

**执行卡：联网构建机；交付目录；发布用户。**

```bash
cd "$UIP_DELIVERY_DIR"
sha256sum --check SHA256SUMS
rsync -av --progress ./ root@<部署机地址>:/root/uip-delivery/
```

SSH 密码只在客户端交互提示中输入。若使用密钥，私钥只保存在受控客户端且权限为 `0600`。

### 4.2 部署机再次校验

**执行卡：离线部署机；`/root/uip-delivery`；root；只读。**

```bash
cd /root/uip-delivery
sha256sum --check SHA256SUMS
for sidecar in *.tar.gz.sha256; do sha256sum --check "$sidecar"; done
sha256sum --check install-host-dependencies.sh.sha256
sha256sum --check install-assets.sh.sha256
```

预期：全部 `OK`。失败时重新传输该文件并再次核对构建端；禁止用部署机重新生成校验值来“修复”不一致。

### 4.3 独立安装入口

当前构建器必须在包外发布两个安装入口及其 sidecar，空白部署机不需要先从尚未校验结构的归档内取脚本：

```bash
cd /root/uip-delivery
sha256sum --check SHA256SUMS
sha256sum --check install-host-dependencies.sh.sha256
sha256sum --check install-assets.sh.sha256
install -d -m 700 /root/uip-bootstrap
install -m 700 install-host-dependencies.sh /root/uip-bootstrap/install-host-dependencies.sh
install -m 700 install-assets.sh /root/uip-bootstrap/install-assets.sh
bash -n /root/uip-bootstrap/install-host-dependencies.sh
bash -n /root/uip-bootstrap/install-assets.sh
```

预期：独立入口同时受总清单和专用 sidecar 约束。缺少该入口或任一校验时，本次交付不完整；不要从未知来源下载同名脚本，也不要用资产包内脚本绕过包外入口缺失。

## 5. 空白部署机准备

### 5.1 使用交付介质安装操作系统依赖

镜像包只包含容器镜像；本交付另行提供宿主依赖归档。它通过本地
`file:` APT 仓库安装，不读取公网源，并严格拒绝不同 Ubuntu `VERSION_ID`、代号或
CPU 架构。由于交付外层归档、SHA 校验和 `.deb` 安装自身存在引导链，Ubuntu 基础
系统仍必须预先提供 `bash`、`tar`、`gzip`、`sha256sum`、`dpkg` 和 `apt-get`。
Docker Engine、Compose plugin、内核模块不在通用依赖包内，必须在断网前按组织批准
的版本单独安装。

构建宿主依赖包的 Ubuntu `VERSION_ID`/代号必须与目标机完全一致。先检查包和目标机：

```bash
cd /root/uip-delivery
cat /etc/os-release
dpkg --print-architecture
sha256sum --check SHA256SUMS
sha256sum --check install-host-dependencies.sh.sha256
HOST_DEPS_PACKAGE=(host-dependencies-ubuntu-*-linux-amd64.tar.gz)
((${#HOST_DEPS_PACKAGE[@]} == 1))
/root/uip-bootstrap/install-host-dependencies.sh "${HOST_DEPS_PACKAGE[0]}"
```

安装入口先校验外层 sidecar、归档成员、内层全部 `.deb` 摘要和包元数据，再使用唯一
本地 APT 源按包名解析；目标机已有更高安全修订版本时不会为匹配归档而强制降级。
如果 OS 不匹配，必须在匹配目标机版本的联网 Ubuntu/amd64 构建机重新执行
`--component host-deps`，不能使用 `dpkg --force-*` 绕过。

安装 Docker Engine 和 Compose plugin 后验证：

```bash
uname -m
docker info
docker compose version
bash -c '((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3)))'
command -v flock ss sha256sum openssl curl tar gzip
command -v jq
```

预期：`x86_64`、Docker daemon 可用、Compose v2 可用、Bash ≥ 4.3。若部署机本来就有业务 Docker 数据，停止并报告，不通过改 `data-root` 或清空 `/var/lib/docker` 假装空白。

### 5.2 安装经过校验的部署资产

**执行卡：离线部署机；`/root/uip-delivery`；root。**

```bash
cd /root/uip-delivery
/root/uip-bootstrap/install-assets.sh \
  "deployment-assets-${UIP_RELEASE_VERSION}.tar.gz" \
  /opt/unified-identity-platform
```

预期：显示安装目标和旧资产备份路径，不启动容器。安装器校验独立 sidecar、归档路径、文件类型和允许列表；已有 Compose 通常保留，新模板写成 docker-compose.yml.dist；安装器也会受控迁移旧 Socket Proxy 镜像、frontend 地址表达式及 runtime-approvals 挂载，须审查实际差异。

若中断后出现 `runtime/.assets-install-transaction`：

```bash
/opt/unified-identity-platform/bin/install-assets.sh \
  --recover /opt/unified-identity-platform
```

预期：恢复事务登记的旧静态资产，保留数据库卷、运行配置和备份。恢复完成后重新执行同一资产安装；不要手工删除事务标记。

### 5.3 放置镜像包

```bash
install -d -m 700 /opt/unified-identity-platform/packages
# 逐一替换为本批次实际文件名，仅复制镜像组件；不要使用会匹配宿主依赖的通配符。
cp -p /root/uip-delivery/common-infrastructure-linux-amd64.tar.gz \
  /root/uip-delivery/common-infrastructure-linux-amd64.tar.gz.sha256 \
  /opt/unified-identity-platform/packages/
# 再按本批次清单复制 platform、frontend 和获批业务组件及其同名 sidecar。
```

不要把 `deployment-assets-*.tar.gz` 放入 `packages/`。预期 `packages/` 中每个组件只有一个候选版本；同组件多个版本会被自动发现逻辑拒绝。

### 5.4 部署资产更新后的控制面成对重载（fail-closed 门禁）

`platform-api` 与 `subsystem-provisioner` 都在进程启动时缓存 `subsystems.d`。如果在控制面运行中执行 `install-assets.sh` 替换部署资产，安装器会留下一个持久门禁文件：

```text
/opt/unified-identity-platform/runtime/.control-plane-reload-required
```

门禁清除前，通常写入口拒绝；doctor/status/logs/help 可诊断。当前 import 只放行经校验的 common/platform，recover-platform 和显式协调的平台发布有专用路径，不能把普通业务 import 或 frontend 当作恢复入口：

- deploy.sh 的 prepare、普通 deploy、continue、upgrade、start、resume、disable、restore、install 等；import 的 common/platform 和 recover-platform 按专用绑定检查放行；
- CI 普通 deploy-service.sh；绑定审核平台镜像的显式协调升级是受控例外；
- 一键部署脚本与 `lifecycle.sh repair`（后者转发到 `deploy-service.sh`）。

被拒绝时输出：

```text
错误：部署资产已更新，但 subsystem-provisioner 与 platform-api 尚未成对重载。
为避免平台与 Agent 使用不同的生产清单，本次操作已拒绝。
请先执行：/opt/unified-identity-platform/bin/deploy.sh reload-control-plane
```

仅普通清单重载使用下述命令；标记绑定新版 PLATFORM_IMAGE／资产摘要时，必须使用本批次平台包走 recover-platform（根目录 forward-recover.sh 包装该流程），不能先让旧二进制解析新清单。普通重载成功后再重试：

```bash
cd /opt/unified-identity-platform
sudo ./bin/deploy.sh reload-control-plane
```

该命令在同一个部署锁内完成：暂停 `platform-api`（避免清单切换窗口接受旧摘要请求）→ 重建 `subsystem-provisioner`（先跑 socket-init）→ 校验 Agent 内 `.release.env`、`docker-compose.yml` 摘要与健康状态 → 重建 `platform-api` → 校验健康与 Unix Socket 可达 → 比对**宿主机、Agent、API 三方的 `subsystems.d` 集合摘要**。全部通过后才删除门禁文件。

- 失败不会删除门禁，可以重复执行；命令会明确指出卡在哪一步。
- 门禁是 `0600` 普通文件，内容含 `ASSETS_ARCHIVE_SHA256`、`INSTALLED_AT_UTC`、`REASON`，可用于确认它对应哪一份资产包。
- 门禁是符号链接或非普通文件时会被直接拒绝；不要手工创建或替换它。
- 控制面本来就没运行时不会写门禁；此时 `reload-control-plane` 只做静态校验并清除残留标记。

> **为什么不能只重启 Agent：** 只重建一侧会让两侧清单集合不同，平台的受控采用/更新会被 `production subsystem manifest drift detected` 正确拒绝，现场表现为“点多少次重试都没用”。

> **旧交付介质必须重新制作。** 本门禁与成对重载只存在于包含该修复的 `deployment-assets` 中。把新脚本单独拷进旧介质、或只替换旧介质 `bin/` 下的文件，都不构成完整修复：旧介质可能仍携带不兼容的 `subsystems.d` 与模板，也不会写入门禁。增量制作（`--extend-from`）在基线不兼容时会直接拒绝复用，此时必须重新构建完整基线。

### 5.5 升级批准替换（维护窗口）

此节是遗留V1四系统批准发布路径；先核实实际平台/Agent版本与登记模式。内置V2普通镜像更新不因摘要变化执行此发布器；不能用V1工具强制覆盖V2身份。

新镜像的运行批准必须随最终包单独交付；资产安装器仍拒绝覆盖不同批准，不能把新批准文件直接塞进 `runtime-approvals/`。本批次 `deployment-assets` 内含 `bin/publish-runtime-approvals.py`，由它在同一部署锁内完成：

1. 校验新批准 bundle 的 `SHA256SUMS`、schema、旧批准 CAS、旧运行镜像和 `upgrade-before.json`；
2. 调用原生 `backup-all.sh` 做全库/配置/文件备份并验证，再独立备份旧批准；
3. 停止 `platform-api` 和 `subsystem-provisioner`，确认没有 UPDATING/VERIFYING/FAILED 业务后原子写入 4 份业务 release 和 `license-evidence.json`；
4. 通过 `lifecycle.sh approval-reload` 成对重载控制面并验证 Agent 只读挂载；
5. 保持 `business_updated:false`、`runtime_ack_observed:false`，业务更新/ACK 另行执行。

服务器 root 终端：

```bash
bundle=/root/uip-final-approval-bundle
deploy=/data/unified-identity-platform
(cd "$bundle" && sha256sum --check SHA256SUMS)
bundle_sha=$(sha256sum "$bundle/SHA256SUMS" | awk '{print $1}')
proof=$(python3 "$deploy/bin/publish-runtime-approvals.py" \
  --deploy-root "$deploy" --bundle "$bundle" --bundle-sha256 "$bundle_sha")
before=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["before_sha256"])' <<<"$proof")
python3 "$deploy/bin/publish-runtime-approvals.py" \
  --deploy-root "$deploy" --bundle "$bundle" --bundle-sha256 "$bundle_sha" \
  --before-sha256 "$before" --apply --confirm PUBLISH_RUNTIME_APPROVALS_MAINTENANCE
```

- 先运行升级入口完成 deployment-assets、平台和前端安装，并把业务镜像暂存（平台停在受控更新等待，不启动新业务镜像）；确认 `bin/publish-runtime-approvals.py` 和带 `approval-reload` 的 `bin/lifecycle.sh` 已就位后执行发布器；再在平台应用页受控更新/重试并完成 ACK。
- 若业务更新先执行并被旧批准拦住，不要手工覆盖批准或删除旧文件；执行发布器后用原升级命令 `--resume` 收口。
- 发布失败时 `platform-api` 保持停止是有意的 containment。保留 `runtime/.runtime-approval-publish`、备份和门禁，使用同一 bundle/哈希执行 `--resume`；不得手工启动 API 或删除事务目录。
- 厂商侧正式批准生成命令和源码分类/目标 facts 前置条件见 [运行组件批准清单教程](../../../sharedDocs/09_商业授权/02_运行组件批准清单.md) 第八节 2026-10-20 小节；候选包不能当正式批准。

## 6. 选择模块与生成配置

### 6.1 整块选择可选模块

生产只使用 `/opt/unified-identity-platform/docker-compose.yml`。公共平台区必须保留；可选块：`contract`、`project`、`customer`、`portal`、`settlement`、`data-analysis`、`monitoring`。

**执行卡：离线部署机；部署根；root；修改前先备份。**

```bash
cd /opt/unified-identity-platform
cp -p docker-compose.yml "backups/docker-compose.before-scope.$(date -u +%Y%m%dT%H%M%SZ).yml"
grep -nE '^# (BEGIN|END) SUBSYSTEM:' docker-compose.yml
```

要排除模块，必须从对应 `# BEGIN SUBSYSTEM: <name>` 到 `# END SUBSYSTEM: <name>` 整块注释，不能只删一个服务。`contract-mysql` 位于公共区并承载 Temporal，不因关闭合同业务而删除。

当前还没有 `.env` 时，不要用裸 `docker compose config` 判断模块；先完成 configure，再运行：

```bash
docker compose --project-directory "$PWD" -f docker-compose.yml \
  --env-file .env --env-file .release.env config --services
```

预期：只列出公共服务和保留模块。解析失败时恢复备份并重新整块编辑。

### 6.2 一次性配置

**执行卡：离线部署机；`/opt/unified-identity-platform`；root；交互。**

```bash
cd /opt/unified-identity-platform
./bin/deploy.sh configure
```

交互项含义：

| 交互项 | 含义 | 注意事项 |
| --- | --- | --- |
| 服务器 IP | 用户实际访问该测试/内网服务的地址 | 必须是合法 IPv4/IPv6；已有平台后不能直接改 |
| 基础平台 API 端口 | 仅宿主机内部管理/健康入口，默认 18080 | 不建议对公网放行 |
| 统一前端 HTTP 端口 | 浏览器平台入口，默认 8081 | HTTP 仅限隔离可信网络 |
| Keycloak HTTP 端口 | OIDC/SSO 公开入口，默认 18090 | 与前端/API 端口不能重复 |
| 管理员账号 | 首个基础平台管理员账户名 | 修改 `.env` 不会给数据库已有账户改名 |
| 时区 | IANA 时区，例如 `Asia/Shanghai` | 服务器必须安装相应 tzdata |
| 启用监控 | 是否启用 monitoring 块 | Compose 中已注释监控块时仍以 Compose 范围为准 |

预期：生成权限为 `0600` 的 `.env`、`.release.env` 和所选模块 runtime 文件；生成签名密钥与随机数据库密码；显示平台和 SSO 地址。UFW/firewalld 只有在已处于 active 状态时才增加配置端口，不会启用或关闭防火墙；云安全组仍需人工确认。

重复执行会复用合法密钥。只有确需重新进入交互时使用 `configure --force`；已有导入/容器记录时仍禁止直接更改 IP/端口。配置中断时正式 `.env` 不应被半成品替换；先运行 `./bin/deploy.sh doctor` 再重试。

### 6.3 安全查看初始管理员凭据

`configure` 生成的初始密码保存在 root-only `.env` 中。只在受控终端显示，不复制到工单、聊天或日志：

```bash
cd /opt/unified-identity-platform
awk -F= '$1=="OFFLINE_ADMIN_ACCOUNT" {sub(/^[^=]*=/,""); print; exit}' .env
awk -F= '$1=="IAM_BOOTSTRAP_ADMIN_PASSWORD" {sub(/^[^=]*=/,""); print; exit}' .env
```

首次登录后通过平台支持的密码修改流程改密。直接编辑 `.env` 中的管理员密码只影响尚未执行的初始化，不会重置数据库中已有账户。

## 7. 平台与前端部署

### 7.1 自动方式（推荐首次安装）

**执行卡：离线部署机；部署根；root。**

```bash
cd /opt/unified-identity-platform
./bin/deploy.sh doctor
./bin/deploy.sh install
```

`install` 先扫描全部包并拒绝歧义，然后按：common → platform → 基础平台健康 → frontend → 公开入口健康 → 子系统 import/prepare 的顺序执行。普通路径只准备候选；匹配签名许可与审核安装计划的专用路径会调用受控采用，不绕过平台接入规则。

预期：基础平台和前端通过健康门禁；已提供且已启用的子系统显示等待平台采用。任何阶段失败都应停止后续阶段，并保留诊断现场。

### 7.2 手工分阶段方式

需要逐步收集证据时执行：

```bash
./bin/deploy.sh import packages/common-infrastructure-linux-amd64.tar.gz
./bin/deploy.sh import packages/platform-backend-<版本>-linux-amd64.tar.gz
./bin/deploy.sh deploy platform
./bin/deploy.sh import packages/frontend-<版本>-linux-amd64.tar.gz
./bin/deploy.sh deploy frontend
./bin/deploy.sh verify
./bin/deploy.sh status
```

预期关键服务：本地 Registry、公共数据库、Keycloak、Temporal、File Gateway、Docker Socket Proxy、Subsystem Provisioner、Platform API/Worker 和 Frontend；健康检查必须为 healthy，Worker 重启计数应稳定。`verify` 同时检查平台 `/readyz`、外部 `/healthz`、OIDC discovery、平台到 File Gateway 的容器网络和匿名文件 API 被正确拒绝。

失败处理：

```bash
./bin/deploy.sh status
./bin/deploy.sh logs platform
docker ps -a --filter 'name=uip'
docker logs --tail 200 <失败容器名>
```

迁移失败容器会保留供检查。不要删除数据库卷，不要把 `Running` 或单个 HTTP 200 当作业务成功。

#### 7.2.1 首次安装的数据库冷启动与等待预算

首次安装时公共 MySQL 需要初始化数据目录并重启一次。实测（16 vCPU / 5.4 GiB 内存的测试机）从容器启动到 `ready for connections ... port 3306` 约 **4.5 分钟**；旧的 180s 等待会在初始化完成前放弃，并把随后必然失败的备份报成"发布失败"，掩盖真正的启动原因。

- 等待预算默认 **600 秒**，可用环境变量 `BASIC_PLATFORM_DB_START_TIMEOUT`（秒）覆盖；慢盘或低配机建议设为 `900`。
- 数据库启动失败时脚本输出：容器 `状态/健康/退出码/重启次数/OOMKilled`、最近 40 行容器日志、宿主机 `free -m`，以及内存基线提示；不再只报 `mysqldump ... socket` 这种误导性错误。
- 首次安装时目标库尚未创建，脚本跳过发布前备份并说明原因；升级发布仍强制备份，备份失败仍中止发布。
- 快速判断是否内存不足（基线 16 GiB）：

  ```bash
  docker inspect -f '{{.State.OOMKilled}} {{.RestartCount}}' <容器名>
  dmesg -T | grep -iE 'oom|killed process'
  free -m
  ```

### 7.3 地址和基础验收

配置完成输出的 `平台：...；SSO：...` 是浏览器地址来源。基础验收至少包括：

1. `./bin/deploy.sh verify` 返回 0；
2. 浏览器打开平台地址并使用初始管理员登录；
3. 能退出并重新登录；
4. 应用接入页面只显示当前已 `prepare` 为不可变 digest 的服务器审核 prod 目标；资产中保留但当前交付未打包/未准备、且没有既有有效环境的应用不会显示；
5. File Gateway 匿名请求被拒绝，而授权后的测试上传、下载需要通过平台 UI/真实业务功能验证。

HTTP Cookie 在网络中明文传输，只能用于隔离验收或可信内网；公网生产必须按第 9 章切换 HTTPS。

## 8. 逐个接入子系统

### 8.0 在已运行环境中后续追加子系统

首次只选择了部分业务系统时，后续追加不能重跑全量首装。使用交付根目录中的
`服务器一键离线部署.sh --add-subsystems <列表>`：它先验证现有基础平台健康，只把指定包
复制到 `packages/`，然后按依赖顺序执行 `import + prepare`。它不执行 `configure`、不替换部署资产、
不重新部署 common/platform/frontend，也不启动尚未完成平台采用的业务服务。

```bash
# 只追加 CRM；先读检，再正式执行
./服务器一键离线部署.sh \
  --media /root/uip-delivery/<发布版本> \
  --target /opt/unified-identity-platform \
  --add-subsystems customer-opportunity \
  --check-only

./服务器一键离线部署.sh \
  --media /root/uip-delivery/<发布版本> \
  --target /opt/unified-identity-platform \
  --add-subsystems customer-opportunity
```

目标同系统如已存在另一版本包，增量模式必须拒绝，并转入第 9 章的受控升级；不得删除旧包后伪装首次追加。
新版子系统如果依赖新的 Compose 服务或清单字段，先使用资产升级流程审核和合并 `docker-compose.yml.dist`；不允许增量模式暗中覆盖现有编排。

### 8.1 推荐顺序和映射

| 顺序 | 包前缀 | CLI 参数 | 平台目标 | 主要运行角色/依赖 |
| --- | --- | --- | --- | --- |
| 1 | `customer-opportunity-backend` | `customer-opportunity` | `customer_and_opportunity/prod` | CRM API、多类通知/商机 Worker；Portal 依赖 CRM |
| 2 | `customer-portal-backend` | `customer-portal` | `customer_portal/prod` | Portal API、邀请补偿 Worker；依赖 CRM |
| 3 | `contract-backend` | `contract` | `contract_management/prod` | 合同 API；`contract-mysql` 也供 Temporal 使用 |
| 4 | `project-backend` | `project` | `project_management/prod` | Project API、SLA Worker；共享 Temporal 依赖先就绪 |
| 5 | `settlement-backend` | `settlement` | `settlement/prod` | 结算 API、Worker |
| 6 | `data-analysis-backend` | `data-analysis` | `data_analysis/prod` | API、聚合/告警 Worker、Metabase |

如果实际业务依赖要求合同先于 CRM，可在不违反 Portal→CRM、Project→Temporal 的前提下调整；验收报告必须记录实际顺序。不要并行采用多个首次接入目标。

### 8.2 每个子系统的固定流程

**执行卡：离线部署机；部署根；root；平台管理员同时在浏览器操作。**

以 CRM 为例：

```bash
./bin/deploy.sh import packages/customer-opportunity-backend-<版本>-linux-amd64.tar.gz
./bin/deploy.sh prepare customer-opportunity
./bin/deploy.sh status customer-opportunity
```

预期：状态提示到平台页面采用 `customer_and_opportunity/prod`，而不是业务容器已经启动。然后由平台管理员在当前环境执行探测/采用；若失败，在同一环境点击重试，不重复创建应用环境。

平台采用完成后：

```bash
./bin/deploy.sh status customer-opportunity
./bin/deploy.sh continue customer-opportunity
./bin/deploy.sh verify customer-opportunity
```

预期：严格经历 `prepare → 平台采用 → continue → verify`。缺少 OIDC、授权目录、审计、File Gateway 等真实接入凭据时，Agent 必须拒绝启动，不能填假值绕过。

数据分析生产运行文件同样必须收到独立的 File Gateway 服务凭据。若 `data-analysis-api`、Metabase 和 Worker 已运行，但页面长期停在“更新中”，且 `verify data-analysis` 报
`FILE_GATEWAY_APPLICATION_ID`、`FILE_GATEWAY_CLIENT_ID`、`FILE_GATEWAY_CLIENT_SECRET` 缺失，说明目标机部署资产中的
`subsystems.d/data-analysis-prod.yaml` 版本过旧或不完整。不要手工向 `runtime/data-analysis.env` 写入密钥；应先升级包含
`file_gateway_write` 白名单及三项运行时绑定的新 deployment-assets，重建 `subsystem-provisioner`，再在原
`data_analysis/prod` 环境点击“重试”。该修复改变共享生产清单，不能通过只复用旧 deployment-assets 的业务镜像增量包交付。

其他系统只替换包名和 CLI 参数。每个系统完成后再开始下一个，并执行：

```bash
./bin/deploy.sh status <CLI参数>
./bin/deploy.sh logs <平台应用编码>
```

### 8.3 业务验收而非容器验收

每个启用系统至少记录：

- 使用对应登录入口完成一次真实登录；
- 创建、读取、修改一条只用于本次验收的测试记录；
- 触发并确认该系统的核心异步任务或 Worker 结果；
- 涉及文件的系统完成一次授权上传、下载和内容核对；
- 涉及跨系统数据的流程验证依赖缺失时显示明确不可用，而不是 504 或无限等待；
- 容器健康、RestartCount、业务日志和追踪号无持续错误。

具体业务字段与预期由业务验收用例定义；`deploy.sh verify` 通过不能替代上述业务验证。

## 9. 日常运维、升级、裁剪与 HTTPS

### 9.1 状态、日志和自检

**执行卡：部署机；部署根；root 或获授权 Docker 运维用户。**

新版交付安装 `系统运维工具.sh` 到部署根目录。甲方日常运维优先使用其中文菜单，或执行无交互巡检：

```bash
cd /opt/unified-identity-platform
sudo ./系统运维工具.sh
sudo ./系统运维工具.sh overview
```

运维工具复用本节原生脚本，不绕过不可变镜像、runtime、备份、许可和健康门禁。2026-10-28加固版的 `overview/resources/collect` 只读取元数据；`doctor/status/verify` 底层可能准备网络或创建一次性验收容器，须明确传入 `RUN_DEPLOY_CHECKS`，例如 `sudo ./系统运维工具.sh status contract RUN_DEPLOY_CHECKS`。核心组件升级实际发布，确认词为 `PUBLISH_CORE_UPGRADE`；业务暂存仍为 `STAGE_UPGRADE`。其他变更保留原精确确认词。数据库恢复、删除子系统、清理环境和删除数据卷不出现在普通菜单中。

```bash
./bin/deploy.sh doctor
./bin/deploy.sh status
./bin/deploy.sh status <子系统参数>
./bin/deploy.sh logs <平台应用编码>
./bin/deploy.sh verify
docker stats --no-stream
```

原生 `doctor/status` 带子系统参数时可经部署阶段检查进入一次性验收，不能一概视为纯只读；`verify` 是健康门禁。需要纯元数据时使用运维工具的 `overview` 或 `collect`。前端不可用时从宿主和同网段客户端分别访问平台 `/healthz`，区分容器、宿主防火墙和上游网络问题。

### 9.2 单系统升级与回滚

```bash
./bin/deploy.sh import packages/<新组件包>.tar.gz
./bin/deploy.sh upgrade <组件参数>
./bin/deploy.sh status <组件参数>
# 回到平台页面对已有 prod 环境执行受控更新/重试
./bin/deploy.sh continue <组件参数>
./bin/deploy.sh verify <组件参数>
```

发布脚本使用不可变 digest、统一部署锁、迁移、健康检查和镜像回退。数据库迁移不会因镜像回滚而自动反向迁移；不兼容变更必须有单独数据库恢复或前向修复方案。失败迁移容器保留，确认日志和退出码后再处理。

容器损坏但不升级镜像时：

```bash
bash ./bin/lifecycle.sh repair <platform|frontend|子系统|all>
```

### 9.3 停用、移除和重新启用

先在平台控制面完成业务退役，再执行：

```bash
./bin/deploy.sh disable <子系统参数>
bash ./bin/lifecycle.sh remove <子系统参数> \
  --confirm REMOVE_<子系统参数>_AFTER_RETIREMENT
```

`disable` 停止模块，`remove` 移除其容器但保留卷与 runtime。重新启用时取消整块注释，校验 Compose，再执行：

```bash
./bin/deploy.sh resume <子系统参数>
./bin/deploy.sh verify <子系统参数>
```

注释 Compose 不会自动停止遗留容器；状态输出的“编排差异”必须处理。Project/Contract 裁剪不能停止共享 Temporal 所需的公共 `contract-mysql`。

### 9.4 HTTPS、证书与资源限制

HTTP 只允许隔离网络。准备证书后先检查：

```bash
./bin/public-transport.sh check
./bin/apply-public-transport.sh
./bin/deploy.sh verify
```

预期：证书链、SAN、私钥匹配和有效期通过，传输状态机完成回调和 Secure Cookie 切换。续期后执行：

```bash
./bin/reload-public-certificate.sh
```

资源限制以 `docker-compose.yml` 为事实来源。修改后先 `docker compose ... config`，再按受控发布路径重建；不要在线直接改容器造成配置漂移。

## 10. 备份恢复与排错

### 10.1 完整备份

**执行卡：部署机；部署根；root；会短暂冻结 File Gateway 写入。**

```bash
install -m 600 /secure/off-host/uip-backup.pass /root/.uip-backup.pass
./bin/backup-all.sh --encryption-key-file /root/.uip-backup.pass
```

预期：生成包含数据库、File Gateway 数据库+文件一致批次、运行配置、签名密钥、TLS、Compose/清单和版本清单的批次；密钥文件与介质分开保管。详细结构见 [BACKUP_RECOVERY.md](BACKUP_RECOVERY.md)。

### 10.2 恢复前只校验

```bash
./bin/backup-all.sh --verify-only \
  --backup <灾备批次目录> \
  --encryption-key-file /secure/separate/uip-backup.pass
```

空目录重建演练：

```bash
install -d -m 700 /srv/uip-rebuild-drill
./bin/backup-all.sh --rebuild-drill \
  --backup <灾备批次目录> \
  --drill-root /srv/uip-rebuild-drill \
  --encryption-key-file /secure/separate/uip-backup.pass
```

预期：只校验或释放到空目录，不修改在线数据。解包成功不等于业务恢复完成。

### 10.3 数据库与 File Gateway 恢复

普通 MySQL 先校验并停止脚本列出的所有写入者：

```bash
./bin/restore-mysql.sh --service project-mysql \
  --backup backups/system/<批次>/project-mysql.sql.gz --verify-only
./bin/restore-mysql.sh --service project-mysql \
  --backup backups/system/<批次>/project-mysql.sql.gz \
  --confirm RESTORE_MYSQL_SERVICE
```

`contract-mysql` 同时承载 Temporal，额外需要：

```bash
--confirm-temporal-maintenance RESTORE_SHARED_TEMPORAL_DATABASE
```

File Gateway 数据库禁止单独恢复，必须使用同批次数据库+文件：

```bash
./bin/restore-file-gateway.sh --backup <网关批次目录> --verify-only
./bin/restore-file-gateway.sh --backup <网关批次目录> \
  --confirm RESTORE_FILE_GATEWAY
```

恢复后必须检查属主/权限、`deploy.sh verify`、测试文件内容、登录、关键读写、Worker 和 Temporal workflow。仅使用本次创建的测试数据做破坏性验收，不覆盖既有未知数据。

### 10.4 常见故障和禁止操作

| 现象 | 下一步 | 禁止操作 |
| --- | --- | --- |
| SHA 校验失败 | 与构建端比对并重新传输 | 在部署机重算清单掩盖损坏 |
| 资产事务未完成 | `install-assets.sh --recover` | 手工删除事务目录 |
| 包架构或 digest 不符 | 隔离该包并重建 | 修改 `package.env` 伪造通过 |
| 端口占用 | `ss -lntup` 找到所有者并评审 | 杀死未知进程 |
| Agent 配置摘要不一致 | 按发布/repair 路径强制重建 Agent | 只做 `docker restart` |
| 提示“部署资产已更新但控制面尚未成对重载” | 执行 `sudo ./bin/deploy.sh reload-control-plane` 后重试 | 手工删除 `runtime/.control-plane-reload-required`、只重启 Agent |
| 受控采用反复报 `production subsystem manifest drift detected` | 先 `reload-control-plane` 让两侧清单一致，再在原环境重试 | 重复新增接入、只重建单个进程 |
| API unhealthy | 看失败容器、迁移容器和 runtime 日志 | 删除数据库卷、吞掉探针 |
| 子系统凭据缺失 | 在原环境修复控制面用途 Client 后重试 | 重复创建应用环境、填占位凭据 |
| 客户门户返回 `PORTAL_AUTHORIZATION_REQUIRED` | 确认 platform、customer-portal 与 deployment-assets 均为包含 000112 的同一新发布；在原 `customer_portal/prod` 环境执行一次受控“更新/重试”，等待 Keycloak 授权投影成功后退出旧会话并重新登录 | 给内部管理员授予 `portal_customer`、直接改授权表、重复创建环境 |
| 前端 504 | 检查目标 API 与依赖健康、网关日志 | 把 HTTP 200 当作业务成功 |
| Worker 重启 | 核对 RestartCount、队列/DB/OIDC 依赖 | 只检查 API 容器 |
| 恢复被写入者阻断 | 停止所有列出的 API/Worker/迁移/Temporal | 绕过锁或直接导入在线库 |

禁止执行全局 `docker system prune -a --volumes`、删除未知卷、清空 `/var/lib/docker`、手工把生产状态改成 READY，或用容器重启代替宿主机重启验收。

客户门户的两类身份不可混用：外部客户由 CRM 邀请流程获得 `portal_customer` 和客户数据范围；受控接入操作人只获得内部 `portal_super_admin`。000112 是旧空角色策略升级的一次性修复：历史库无法区分“从未授予”与“曾手工授予后撤销”，因此升级后的首次受控更新/重试可能补发一次内部角色；补偿写回完成 marker 后，后续日常更新不会恢复再次被管理员主动撤销的角色。

## 11. 验收记录与完成标准

每次发布至少保存以下脱敏证据：

- 源码指纹、发布版本、构建开始/结束时间、构建工具版本；
- 每个包大小、包 SHA、组件/版本/平台、镜像 config 摘要；
- 部署机只读预检、Compose 实际服务列表、本机 Registry 中发布 digest；
- 平台、前端和每个子系统的真实登录、关键读写、文件与异步任务范围；
- 故障演练、恢复批次、恢复后业务核对和清理结果；
- 所有阻塞项、未执行项及原因。

脚本单测、Compose 解析或文档推演不能写成真实部署通过。宿主机重启、长期稳定性、外部安全组和跨网络客户端未执行时分别标记“未执行”；不能用容器重启代替。

## 附录 A：同一台空白测试机的两阶段验收

同机测试仍要保持“构建输出”和“部署输入”隔离：

1. 在联网阶段只使用源码目录和默认构建 Docker，产物写入 `/srv/uip-delivery/<版本>`；完成第 3 章全部校验。
2. 把产物复制到只读验收目录 `/srv/uip-acceptance-media/<版本>`，记录复制前后 SHA；部署阶段只读取该目录，不引用源码内 Compose、脚本或镜像标签。
3. 不清空全局 Docker 数据。需要验证“无构建缓存”时，使用独立 Docker daemon 的独立 `data-root`、`exec-root`、PID 和 Unix socket；启动前保存 `docker ps/volume ls`，发现已有未知 UIP 资源立即停止。
4. 网络隔离必须只作用于测试 daemon 所在的 network namespace 或专用测试网段，保留宿主 SSH 和内网；先写自动回滚，再施加规则。确认测试 daemon 的公网拉取失败、本地 Registry 可用后再部署。
5. 若隔离环境不能提供真实浏览器回调、邮件、短信或外部对象存储，就把对应业务验收标为阻塞，不能用 mock 结果替代。
6. 验收结束恢复网络限制，停止独立 daemon，保留本次发布包、脱敏报告和获批的测试数据；是否删除测试容器/卷按验收清理单执行，不能触碰构建 Docker 的既有资源。

验收服务器 `192.168.31.153` 可按本附录执行；SSH 密码只用于交互认证。本手册不记录密码，也不对 `192.168.3.38` 执行任何操作。

## 附录 B：发布前最小核对清单

```text
[ ] 构建/部署机预检已保存且无未知既有资源
[ ] 全部自研镜像实际构建为 linux/amd64
[ ] 9 个镜像包 + 1 个宿主依赖包 + 1 个资产包 + 2 个独立安装入口 + 全部 sidecar + SHA256SUMS 齐全
[ ] 构建端和部署端 SHA256SUMS 全部 OK
[ ] 资产包不含运行 Secret、私钥、备份或业务数据
[ ] 从资产包独立取得 install-assets.sh 并从空部署目录安装
[ ] configure、平台、前端按阶段成功且 verify 返回 0
[ ] 每个启用子系统按 prepare → 平台采用 → continue → verify 完成
[ ] 真实登录、关键读写、文件、异步任务范围已记录
[ ] 故障、安装中断、损坏包和恢复演练结果已记录
[ ] 网络限制已恢复；未执行项和外部阻塞已单列
```

## 2026-10-13 全链路核验补充

本手册的构建机／生产运维／应用管理员／业务复核人角色及步骤保持区分；本次只核验源码，不声明新动态验收。
近期工作区未提交脚本改动也是本次事实来源；正式介质必须重新构建，不能因为本地说明更新就认定服务器有新能力。

1. 中文一键入口自动判断空安装／旧安装；残留部署标识不完整时拒绝猜测。
   --check-only 检查介质、系统、资源并在隔离 scratch 中试装资产，不修改现有部署，但会创建临时文件。
   --upgrade 不是 dry-run：先调用原安装的 backup-all，再导入及安装资产，最后才展示／合并差异。
   --yes 仅资源提示，不代表升级或差异审批；非交互合并须在审阅后明确 --apply-compose。
2. install 普通路径依次扫描包、导入 common/platform、发布平台并检查、导入发布 frontend，再准备业务候选。
   带匹配 license-dir／审核 license-plan 时另执行专用许可安装和自动采用路径；不把这条路径改写成“永远只 prepare”。
   本次只核实辅助脚本调用，商业授权政策由原授权文档负责。
3. prepare 在范围锁内保存摘要、刷新控制面并创建 network=none 的停止候选；不启动业务数据库。
   continue 仅允许已由后台采用的 READY_TO_CONTINUE，删除候选后完成技术阶段；不做新部署或迁移。
   VERIFIED 不等于期望新 digest 已在所有服务生效，仍须 verify、实际容器摘要和业务验收。
4. 平台／前端 upgrade 实际发布，业务 upgrade 先暂存后从页面更新。
   start 和 resume 会调用发布、备份、迁移、目录同步及健康等待，不能当作单纯 docker start；
   resume 还要求已接入、当前运行容器和本地镜像满足检查。
5. 受控采用／更新要求对应租户的应用、环境、登录目标更新及 OAuth disable 权限；
   重试另需 role-binding:update。后台事务锁定代次，写入凭据、desired manifest、Agent 更新、迁移及所有声明 API／Worker，
   最后写 applied manifest／初始权限及 READY；受理响应不代表完成，失败查看终态错误并保持数据。
6. 底层发布使用统一部署锁及完整备份；控制面更新须平台 API／Agent 同版本。
   资产安装只备份静态文件并记录持久事务；残留事务先 install-assets.sh --recover，数据库恢复不属于该回滚。
   HTTPS／排空不加载旧覆盖层，runtime/compose.timezone.yaml 是新时区流程例外，避免裸 Compose 丢失设置。
7. 基础 verify 包含健康、discovery 与匿名网关接口 401／405，不做真实登录、IAM 权限、业务读写或 Workflow；
   Keycloak TCP 9000 探测不能替代 HTTP ready。
8. 备份跨库非同一时点，状态归档加密不包括 SQL／网关文件；网关停写时长取决于数据量。
   隔离网关 drill 使用平台工具、临时 1 GiB MySQL 和最多 100000 条 READY 对账；核对扫描总数。
   网关 SQL 部分导入时文件回退不等于 SQL 回滚，保持停写；详见 [BACKUP_RECOVERY](BACKUP_RECOVERY.md)。
