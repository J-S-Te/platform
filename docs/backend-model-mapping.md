# 前端页面与后端模型映射

## 1. 接口通用约定

- 根路径：`/api/v1`；JSON 使用 `snake_case`；时间使用 RFC 3339 UTC 字符串；主键使用 ULID。
- 成功响应：`{ "code": "OK", "message": "...", "request_id": "...", "data": {} }`。
- 列表响应：`data.items`、`data.page`、`data.page_size`、`data.total`。
- 失败响应：`{ "code": "AUTH_PERMISSION_DENIED", "message": "无权执行此操作", "request_id": "...", "details": {} }`。
- 未实现的 P1 能力返回 `501` 与 `PLATFORM_CAPABILITY_NOT_ENABLED`，不得以 mock 成功响应掩盖未启用能力。

## 2. IAM 与授权

| 前端实体/动作 | 后端资源 | 表/聚合 | 权限示例 |
|---|---|---|---|
| 用户列表、详情、新增、编辑、启停 | `/users` | `iam_user` | `platform:user:view/manage` |
| 登录账号、账号状态 | `/accounts` | `iam_account`、`iam_password_credential` | `platform:account:view/manage` |
| 组织、岗位、任职 | `/org-units`、`/positions`、`/memberships` | `iam_org_unit`、`iam_position`、`iam_membership` | `platform:organization:view/manage` |
| 角色与权限集合 | `/roles` | `authz_role`、`authz_role_permission` | `platform:role:view/manage` |
| 角色绑定 | `/role-bindings` | `authz_role_binding` | `platform:role-binding:view/manage` |
| 权限注册 | `/resources`、`/permissions` | `authz_resource`、`authz_permission` | `platform:permission:view/manage` |

账号、用户、任职、角色绑定是不同模型；账号禁用不删除用户，任职结束不删除历史记录。所有更新请求携带 `version`，后端使用乐观锁。

## 3. 审计日志

### 3.1 查询 DTO

当前控制台允许展示：`id`、`occurred_at`、`operator_display_name`、`action_type`、`application_name`、`environment_code`、`resource_display_name`、`resource_type`、`method`、`path`、`client_ip`、`status_code`、`result`、`risk_level`、`detail`、`change_summary`。

当前控制台不展示且导出 DTO 不返回：`subject`、`request_id`、`trace_id`。这些字段仍按审计规范保存在内部模型，供受限排障和跨系统关联使用。

### 3.2 约束

- 审计列表支持 `keyword`、应用、环境、动作类型、风险、结果和时间范围筛选。
- 审计事件只读；没有 `DELETE /audit/events/{id}` 或批量删除接口。
- 导出通过 `POST /audit/export-jobs` 创建 `async_job`，随后查询任务状态/下载地址；不得在同步请求直接生成大文件。
- 审计写入 API 要求稳定 `event_id` 并以 `audit_event_dedup` 幂等。

## 4. 设置与 P1 预留

| 前端页签 | 接口模型 | 状态 |
|---|---|---|
| 基础设置 | 配置命名空间 `platform.console`，条目/发布模型 | P0 |
| 通知设置 | `notification_setting` 逻辑模型，只支持 `inbox`、`email` | P1 |
| 字典管理 | `dictionary`、`dictionary_item` 逻辑模型 | P1 |
| 登录安全 | `sec_login_attempt`、`sec_mfa_factor`、`sec_risk_event` | P1 |
| 审计投递 | 平台接收侧投递统计/死信重放 | P1 |
| 可观测性 | `obs_service`、`obs_log_policy`、`obs_alert_rule` | P1 |

安全设置中的“失败次数阈值、锁定时长、重置窗口、IP 限流、MFA、失败关闭”归入 P1 安全策略；平台不保存实际运行日志、Trace 或 Metric，只保存服务和告警配置元数据。

## 5. 前端调用说明

- 前端请求必须发送 `Accept: application/json`；有请求体时发送 `Content-Type: application/json`。
- 认证后所有请求使用 `credentials: 'include'`；不得从 LocalStorage 读取或写入 JWT。
- `401` 表示会话失效，前端跳转登录；`403` 表示权限拒绝；`409` 表示版本冲突，提示刷新；`429` 表示限流；`501` 表示 P1 能力尚未启用。
- 前端在替换 mock 数据前，必须将响应中的 UTC 时间按页面时区格式化，不得修改后端原始时间。
