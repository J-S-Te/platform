# 离线时钟恢复工具

新构建的部署资产包携带 `bin/clock-recovery`（静态 Linux/amd64 可执行文件）及本说明。
通过原有介质验签、资产校验和 `install-assets.sh` 安装后，目标服务器无需 Go 或网络即可运行。
查看工具版本：

```bash
/opt/unified-identity-platform/bin/clock-recovery version
```

恢复工具只用于已获授权组件的时钟回拨恢复，不延长业务授权、不改变许可证版本，
也不能替代介质验签、运行许可或组件真实 ACK。禁止向既有签名介质原地插入本工具或重新签名；
旧环境如需该能力，应使用另行构建、审核和签名的新交付物，按受控流程安装。

## 交付与信任材料

- 时钟恢复交付内容不包含厂商私钥、平台私钥、组件状态文件或一次性恢复凭据。
- 客户侧使用平台公钥与厂商公钥；公钥应通过既有独立信任渠道交付并核对身份。
- 厂商签发操作在厂商持钥的受控机器上执行。运行 `sign` 需要私钥，不在客户服务器上签发。
- 每个组件使用自己的认证状态与恢复包。不得用一个 API/Worker 的恢复包处理另一个组件。
- 恢复包、准备回执、完成回执和状态备份均含运维信息，应存放在访问受限目录，按受控渠道传递。

## 厂商预约签发

厂商先依据指定组件的认证状态确认组件、许可证和授权到期时间，再预约现场恢复时刻。
预约时间必须使用含时区的 RFC3339，例如 `2026-11-20T14:30:00+08:00`。
以下路径均为示例，须替换为本次已确认组件和受控文件路径：

```bash
clock-recovery sign \
  -state /secure/input/component-state.json \
  -platform-public /secure/trust/platform-public.pem \
  -vendor-public /secure/trust/vendor-public.pem \
  -private-key /secure/vendor/vendor-private.pem \
  -expect-application contract_management \
  -expect-service contract-worker \
  -anchor-at '2026-11-20T14:30:00+08:00' \
  -out /secure/output/NEW-recovery-package
```

不传 `-anchor-at` 时沿用立即恢复。凭据签发时间取厂商真实时钟；预约不能早于签发时间，
也必须仍在该组件的业务授权期内。凭据截止时间为预约锚点后 30 分钟；
实际应用仍须满足本地观察时钟与锚点相差不超过 300 秒，30 分钟不是放宽现场操作窗口。
将完整恢复目录通过受控渠道交给客户，保留 token、组件元数据与其签名，禁止拆分或修改。
目录包含 `recovery.jws`、`recovery-metadata.json` 和 `recovery-metadata.sig`；
独立签名元数据绑定组件身份、状态基线及本次恢复凭据。

## 客户现场恢复

恢复前先校准服务器可信时间，按现有运维流程停止使用该状态的对应组件，
确认本次恢复已授权、组件身份与状态路径一致，并为原状态及回执目录准备受限权限。
不能通过删除状态、改写时间高水位或复制其他组件状态绕过时钟保护。

执行前先在目标 Linux 主机核对状态及其稳定锁文件的实际数字 UID/GID 和权限：

```bash
stat -c '%u:%g %a %n' /secure/component-state.json /secure/component-state.json.lock
```

两个文件应属于该组件实际运行身份，权限均为 0600；路径或属主异常时先核对现有部署。
必须以该实际 UID/GID 运行恢复工具，并将工具、公钥、完整恢复包、备份及回执目录放在
该身份可访问的受限位置。`RecoverClock` 的原子替换不保留原文件 UID/GID，
以其他身份运行可能把状态改为其他属主，导致组件之后无法读取。
合同组件默认以 root 运行；项目、结算、报销等可能使用非 root 身份，
不能一概使用宿主机 root 恢复，也不能仅凭镜像默认值推断当前文件所属身份。
以下 `inspect`、`apply`、`receipt` 命令均应在上述已核对的组件 UID/GID 下执行；
身份切换沿用现场已有的受控运维方式。不要删除或替换稳定 `.lock` 文件。

可以先执行只读检查：

```bash
/opt/unified-identity-platform/bin/clock-recovery inspect \
  -state /secure/component-state.json \
  -platform-public /secure/trust/platform-public.pem \
  -vendor-public /secure/trust/vendor-public.pem
```

在约定时间窗口使用整个恢复目录，并显式指定组件回执：

```bash
/opt/unified-identity-platform/bin/clock-recovery apply \
  -state /secure/component-state.json \
  -package /secure/NEW-recovery-package \
  -receipt /secure/receipts/contract-worker-recovery.json \
  -platform-public /secure/trust/platform-public.pem \
  -vendor-public /secure/trust/vendor-public.pem \
  -confirm
```

工具在恢复前耐久写入准备回执并备份原状态，恢复后读回核销记录，再原子写入组件完成回执。
`-receipt` 可省略，默认写为 `STATE.recovery-RECOVERY_ID.json`，文件权限为 0600；
现场宜显式指定，以便与组件及交付工单对应。`-backup` 可另行指定状态备份路径。
先确认命令成功和完成回执，再按既有流程启动原组件并检查业务许可状态及自然 ACK。
启动前再次执行上述 `stat`，确认状态与 `.lock` 的数字 UID/GID 保持正确，且权限仍为 0600。
不能把状态恢复成功等同于组件业务运行成功。业务许可证已经过期时，时钟恢复不会恢复过期业务资格。

## 中断与回执补齐

同一恢复 ID 只能核销一次。应用失败时保留原始恢复目录、准备回执、备份和错误信息，
先判断是否已经耐久核销。不要反复执行 `apply` 或删除核销记录。
若状态已经核销、仅完成回执写入失败，应使用 `receipt` 入口依据耐久记录补齐；
该入口不再次恢复时钟或消费恢复 ID，沿用原组件、恢复目录和回执路径：

```bash
/opt/unified-identity-platform/bin/clock-recovery receipt \
  -state /secure/component-state.json \
  -package /secure/NEW-recovery-package \
  -receipt /secure/receipts/contract-worker-recovery.json \
  -platform-public /secure/trust/platform-public.pem \
  -vendor-public /secure/trust/vendor-public.pem
```

回执从 `PREPARED` 更新为 `RECOVERED` 才表示工具确认本次恢复。
回执中的 `business_allowed` 是核验时依据原许可证、耐久时间高水位与原时钟规则计算的只读结果，
不表示审批成功、外部系统调用成功或业务验收通过；它不能替代组件启动后的许可与自然 ACK 验证。
`RECOVERED_REBLOCKED` 表示已核销后再次锁定；`RECOVERY_UNCERTAIN` 表示恢复结果不确定；
`NOT_APPLIED` 表示未应用（例如备份失败）。这些状态不能当作已恢复成功，须保留证据后继续排查。
旧凭据仍可用 `-token FILE` 入口，`-token` 与 `-package` 必须二选一；
新签名恢复包应始终使用 `-package`，不能拆出 token 来省略组件和包完整性校验。

## 构建工作站

资产构建沿用 `platform/deploy/production/bin/build-offline-packages.sh --component assets`。
构建工作站需要与工程 `go.mod` 兼容的本机 Go 工具链；构建器关闭工作区覆盖及全局 Go 配置，
使用只读模块清单直接编译工程根 `commercial-license-tool/cmd/clock-recovery`，并要求本地
`license-core` 替换指向同一工程根。源码或依赖缺失、Go 不兼容、非静态产物均阻断资产发布。
shared/full 输入指纹纳入工具源码、Go 模块清单、共享许可库源码和本说明；构建记录保留实际 Go 版本。
新资产继续使用原有 SHA256、发布签名、结构检查和安装证明链路。
