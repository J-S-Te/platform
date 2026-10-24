# 在远程服务器运行本地统一编排

> 源码核验：2026-10-13。本页仅用于获授权的开发／演示服务器，不是生产离线部署手册。
> 生产使用 [OFFLINE_RUNBOOK](../deploy/production/OFFLINE_RUNBOOK.md)。

## 角色、条件与边界

服务器运维确认目标机、Docker 权限、可信网络和测试数据范围；应用管理员完成所属租户接入；
业务人员验证真实登录／权限／任务。不得照抄历史服务器地址，也不得自动删除已有数据。
服务器需相邻七仓库：platform、frontend、contract_management、customer_and_opportunity、
project_management、Settlement、data_analysis，以及脚本需要的配置、Docker／Compose、构建依赖和网络。
这是源码构建路径：未预加载镜像时会构建／拉取，不能声称天然断网可用。

## 操作步骤

1. 将获批源码版本按相邻目录传到测试服务器；传输前排除实际 `.env`、`.env.local`、`.env.presale-worker`、
   私钥、data、backups 和未审查日志。不能直接 rsync 整个开发工作区连同 Secret。
2. 经受控渠道准备本服务器独立配置，保留已有库对应的密码和加密／签名密钥；实际文件 0600。
   售前 Worker 的独立环境文件不由启动脚本生成；已有卷但原配置丢失时停止并恢复配置。
3. 如已有平台可用，先完成有效应用接入；合同严格启动条件不接受占位 Client。首次平台初始化须按实际环境拆分核实，不能绕过校验。
4. 在 platform 目录执行：

```bash
bash scripts/docker-local.sh up
bash scripts/docker-local.sh ps
bash scripts/docker-local.sh verify
bash scripts/docker-local.sh logs api subsystem-provisioner
```

首次管理员交互输入；自动化仅用 `--admin-password-stdin`。
`up/restart` 会重走构建、迁移和分阶段启动，不是安全的只读检查或单纯重启。
需要使用已加载本地镜像时可显式 `SKIP_IMAGE_PREPARATION=1`，但必须先确保所有所需 `:local` 镜像齐全且版本一致；
这不转换为生产不可变摘要发布。

## 访问与成功判断

默认 frontend 在回环端口 8081；对远程访问先由运维批准绑定和防火墙。
`lan-access.sh enable --address <获批地址> --port 8081` 只覆盖平台、合同、CRM 和统一前端；
不自动重建 Portal／项目／看板／Keycloak。它设置平台 HTTP Issuer，不能保证原 Keycloak 登录无须其它变更。
公开访问、TLS、公网回调及 Secure Cookie 使用生产部署模型，不开放数据库或 Docker 代理到公网。

`verify` 只给出路由／匿名状态／健康／登录重定向检查；还需从实际客户端测试 Issuer、登录、租户权限、
各启用业务模块及 Worker 的最终结果。Portal／项目／看板有配置和接入门禁，静态页不代表服务已上线。

## 备份与恢复

Docker 命名卷和对象目录需要独立受控备份；直接复制运行中 MySQL 卷不是一致性备份。
本地脚本不提供生产 `backup-all` 的完整批次协议，不要在本地目录直接调用生产恢复器。
维护前识别实际 Compose project、服务、卷和文件目录；审批停写后使用对应数据库逻辑导出，
并将配置／密钥与数据库／文件恢复点配套保存。数据库密码只在容器内读取，不能放 `docker exec -e MYSQL_PWD=<明文>` 参数。

涉及 File Gateway 的恢复必须让数据库和正式／隔离对象目录来自一致批次；临时上传排除策略需明确。
只有在隔离环境验证数据库导入、密钥匹配、READY 文件对账、登录和业务后，才可批准在线恢复。
生产灾备命令与局限见 [备份恢复](../deploy/production/BACKUP_RECOVERY.md)，其目录和锁协议不能假设与本地编排相同。

## 自启与日常运维

已有且正确创建的 `unless-stopped` 容器可随 daemon 恢复；这不会补齐未接入模块或迁移新版本。
不能未经审批把 `docker-local.sh up` 作为每次开机的无人值守任务：它可能构建、拉取、迁移并要求输入／有效凭据。
若需要 systemd 包装，先评审环境、启动顺序、网络依赖、Secret 权限和失败策略，本页不提供未经验证的自动上线单元。

按 [本地操作说明](local-docker-operations.md) 使用定向更新；`ps/logs/verify/config` 的环境准备也可能写文件。
失败保留状态，查看脱敏日志；`stop` 保留数据，`down` 移除容器与 orphan，
`down --volumes` 只有批准销毁测试数据库时才能执行。禁止以重建卷、重新接入或关闭鉴权作为故障恢复。
