# CRM 更新时权限目录哈希不匹配（2026-09-07 归档）

## 原因与证据

用户报告追踪号 `01M1WXYTH4NAAA9FVCC26B2CA3` 对应的应用更新失败。现场 CRM API 重启循环，其他 CRM worker 已启动；API 日志明确提示 `OIDC role configuration hash does not match embedded authorization catalog`。

运行镜像为 `sha256:de14857b55877f966409c100373a54e4411fb479e5345c134752924c2bd37e6c`。通过该镜像的只读 `authz-catalog crm` 验证，其内嵌目录为 `crm-2026.08.28-v10`，兼容哈希为 `sha256:3121000b3a3242b3005ca9a79e71a47893b270cb58fddc770dcc163db04d7524`，最大角色数为 10。

服务器生产清单与 runtime/customer.env 均仍为旧哈希 `sha256:77443efe31deec9ade8836e826b7240edfc377e953b9f5722e37dace011db0bb`。生产接入更新按清单生成运行值，与直接发布脚本从实际镜像读取哈希的路径不一致。磁盘仍有约 23GB 空闲，不是本次失败原因。

## 已执行修复

- 服务器配置原件备份至 `/opt/basic-platform/backups/crm-catalog-repair-20260907-4SnP2ELO/`，含敏感运行配置，目录权限 0700，未下载备份内容。
- 将生产清单和 CRM 运行配置中的旧哈希精确替换为实际镜像输出；角色数量已一致，无需改变。未关闭兼容性校验。
- Compose 配置静态校验通过；仅重建 customer-api，保持原镜像，等待健康检查成功。
- 重启 subsystem-provisioner 与 platform-api，重新加载生产清单。
- 同步仓库 `deploy/production/subsystems.d/customer_and_opportunity-prod.yaml`，与当前工作区 `go run ./cmd/authz-catalog crm` 输出一致。
- 平台 applicationregistry/infrastructure 包测试通过。

历史失败操作记录未直接改库清除；如页面仍展示旧失败，需要在修正后的清单下重新执行正常重试流程。未来目录版本变化仍需同步清单，不能将本次固定哈希修正描述为已实现所有镜像版本的动态协调。
