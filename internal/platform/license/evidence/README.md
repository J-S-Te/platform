# 商业授权可信部署证据说明

完整中文说明已迁移至共享文档库，请阅读：

[可信部署证据说明](../../../../../sharedDocs/09_商业授权/04_可信部署证据说明.md)

本目录保留证据采集实现；本文件仅提供原入口导航，正文只维护共享文档库中的一份。

本页下段保留 2026-10-13 的 V1 证据边界；当前登记模式须另查 LEGACY_IMAGE_V1/BRIDGING_COMPONENT_V2/BUILTIN_COMPONENT_V2。内置目录协议 2 不把普通镜像重建当新业务组件身份；发行完整性/部署证据与商业许可证仍为不同事实。当前操作以共享教程和 [受控登记说明](../../applicationregistry/application/RUNTIME_LICENSE.md)为准。

历史 2026-10-13 核验：持有许可证 read 的管理员从 runtime_handler 的应用/安装证据接口发起查询 → Unix Socket 固定 LICENSE_EVIDENCE/INSTALLATION/MIGRATION 动作 → applicationregistry 读取固定批准 → 本目录 collector/installation/migration/infrastructure 只读采集。前提是受控 Agent、真实完整项目边界及不可变镜像批准，runtime-approvals 目录存在时禁止回退根批准。成功须报告正确 scope、complete、boundary_supported 且无 problems；HTTP200不代表资格。失败查批准权限、完整成员、镜像/副本及 BI 网络隔离。升级前 MIGRATION_INSTALLATION 和目标 INSTALLATION 不混用，冻结另经 coordination/baseline.go 和迁移119。本次未采集真实Docker或迁移。
