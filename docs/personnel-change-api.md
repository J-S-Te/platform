# 人员异动中心接口契约

人员异动中心由平台统一维护人员任职、审批和生效状态。子系统只负责发布责任对象快照，不直接修改平台任职记录。

## 状态机

```text
DRAFT -> PENDING_APPROVAL -> PENDING_HANDOVER -> SCHEDULED -> EXECUTED
   |           |                    |             |
 CANCELLED  REJECTED            CANCELLED     CANCELLED
```

`SCHEDULED` 只能在审批编号存在、生效时间有效且离职交接检查通过后进入。定时 Worker 在 `effective_at <= now` 时请求 `EXECUTED`，仍会再次执行交接和生效校验。

## 端点

```text
GET  /api/v1/personnel-changes?status=<状态>
POST /api/v1/personnel-changes
POST /api/v1/personnel-changes/preview
POST /api/v1/personnel-changes/:change_id/transition
GET  /api/v1/personnel-changes/:change_id/preview
```

异动申请字段：`user_id`、`source_membership_id`、`target_org_unit_id`、`target_position_id`、`change_type`、`reason`、`approval_reference`、`effective_at`。

`change_type` 支持 `PROMOTION`、`DEMOTION`、`TRANSFER`、`TERMINATION`、`REHIRE`。

列表和转换成功响应中的异动对象至少包含：`id`、`tenant_id`、`user_id`、`change_type`、`status`、`reason`、`approval_reference`、`effective_at`、`submitted_by`、`approved_at`、`executed_at`、`version`、`created_at`、`updated_at`。

## 责任交接

CRM、合同和审批适配器向 `iam_personnel_handover_item` 发布责任快照。每项包含：

- `system_code`：`customer_and_opportunity`、`contract_management` 或 `approval`；
- `resource_type` / `resource_id`：业务对象类型和 ID；
- `current_owner_id` / `target_owner_id`：原负责人和接收人；
- `status`：`PENDING`、`TRANSFERRED`、`COMPLETED` 或 `BLOCKED`。

离职异动存在 `PENDING` 或 `BLOCKED` 项时，接口返回 `409`，不会进入 `SCHEDULED` 或 `EXECUTED`。交接检查服务不可用或查询失败同样按安全失败处理。`TRANSFERRED` 和 `COMPLETED` 不再阻断离职。

权限影响预览返回：

```json
{
  "added_roles": [],
  "removed_roles": [],
  "retained_roles": []
}
```

状态展示应以服务端 `status` 为准，不根据本地时间推断“已执行”，也不把 `PENDING_HANDOVER` 显示为“已完成”。
