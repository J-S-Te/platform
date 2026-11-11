# 工程根离线交付工具源码

`engineering-root/` 版本化保存工程根目录的离线制作、服务器部署和配套工具源码。
本次收录保留原工程根文件，复制件与原件逐字节一致；没有改写脚本的路径解析或运行行为。
`SHA256SUMS` 仅覆盖下列 11 个白名单源码文件，路径相对于本目录。

| 路径 | 用途 |
| --- | --- |
| `一键制作离线镜像包.sh` | 工程根离线制作入口 |
| `服务器一键离线部署.sh` | 服务器离线安装、升级和恢复入口 |
| `系统运维工具.sh` | 已安装系统的受控运维入口 |
| `清理首次部署产物.sh` | 失败首装盘点及受控清理入口 |
| `license-activate.sh` | 调用平台受控授权流程 |
| `license-upgrade.sh` | 平台授权组件配套升级入口 |
| `forward-recover.sh` | 平台组件前向恢复入口 |
| `整理客户交付目录.sh` | 整理已核验的客户交付目录 |
| `scripts/check-session-governance-sync.sh` | 同级业务仓库会话治理副本检查 |
| `scripts/session-governance.sha256` | 会话治理副本校验清单 |
| `操作说明/失败首装清理与重新部署.md` | 失败首装清理与重新部署说明 |

表中路径均位于 `engineering-root/` 下；README 和校验清单本身不是复制的工程根源码。

## 使用布局与 CI

这些脚本须在工程根布局中使用。制作入口通过自身位置查找
`platform/deploy/production/`，并按所选组件读取同级仓库：

```text
<工程根>/
  一键制作离线镜像包.sh
  服务器一键离线部署.sh
  scripts/
  操作说明/
  platform/
  frontend/
  customer_and_opportunity/
  contract_management/
  project_management/
  Settlement/
  data_analysis/
```

独立 platform CI 应把 `engineering-root/` 的内容复制到临时工程根，
在该临时根中用 `platform` 符号链接指向本次待验证的 platform checkout，
按定向门禁需要准备其余同级仓库或测试夹具，并设置：

```bash
export UIP_ENGINEERING_ROOT="<临时工程根绝对路径>"
```

`UIP_ENGINEERING_ROOT` 用于门禁定位准备好的工程根；原根脚本仍依据自身位置定位，
因此仅设置环境变量、未准备上述布局不能替代实际依赖。不要直接在
`platform/tools/offline-delivery/engineering-root/` 内运行正式制作或部署。
日常仍可沿用原工程根入口；本目录提供可审查、可复现的版本化源码。

校验收录文件内容：

```bash
cd platform/tools/offline-delivery
sha256sum --check SHA256SUMS
```

macOS 可使用 `shasum -a 256 --check SHA256SUMS`。
后续修改原工程根工具时，应明确核对并更新本目录对应源码和清单，
避免把旧交付包中的脚本当作工程根的当前版本。

## 外部依赖与收录边界

本目录不是自包含离线介质。platform 已有的 `deploy/production/bin/` 公共工具
继续由本仓库提供，不在这里重复复制；业务源码和镜像构建输入来自上述同级仓库。

按所用功能，`commercial-license-tool/`、发布验证器、公开信任材料、正式签名和
运行批准材料仍是受控的外部依赖。脚本中的 `Key/公钥`、`outs/` 等默认路径只是
现有接口约定，不表示本目录提供这些材料；缺少必需材料时保留原有失败门禁。

本次采用明确文件白名单，没有收录 `Key/`、私钥、Token、运行配置、`runtime/`、
数据库或配置备份、`.artifacts/`、`outs/` 离线介质及客户材料、厂商签发工具私有材料。
没有收录旧 V0.1 入口或额外的历史 preflight 脚本，也没有创建父工程 Git 仓库。
