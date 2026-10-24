# 商业授权运行时协调与激活说明

完整中文说明已迁移至共享文档库，请阅读：

[运行时协调与激活说明](../../../../../sharedDocs/09_商业授权/03_运行时协调与激活说明.md)

本目录保留协调器实现；本文件仅提供原入口导航，正文只维护共享文档库中的一份。

2026-10-22 组件登记补充：当前 API 装配内置 catalog，登记协议 2/catalog version 1，签名快照/JWS仍协议1。新安装和旧V1由受控 ReconcileBuiltinComponents 登记/桥接，普通重建不改变 V2 组件身份；LEGACY_IMAGE_V1/BRIDGING_COMPONENT_V2/BUILTIN_COMPONENT_V2 的持久状态与商业强制执行状态分开。当前入口见 [受控登记说明](../../applicationregistry/application/RUNTIME_LICENSE.md)。

历史 2026-10-13 核验：管理员在“运行时授权执行”选系统、刷新、勾确认后“确认激活”；前提为read/manage、有效许可证和完整受控登记。RuntimeEnforcementView/runtime-api → 用户/机器鉴权路由 → runtime_handler → service/lifecycle/baseline 的行锁事务和迁移117–119 → consumer/syncclient持久应用后ACK。首次全部APPLYING确认会切ENFORCED并再次提升修订；受控更新已ENFORCED应用保留策略并清旧快照/ACK。成功须snapshot_current=true及非空完整当前集合issued_revision=ack_revision=应用revision、准备/确认时间存在；状态文字和计数不足。失败刷新CAS或修真实凭据、批准、时间/持久卷，再重试；不可填假ACK、清表或退回兼容。本次未运行协调/数据库验收。
