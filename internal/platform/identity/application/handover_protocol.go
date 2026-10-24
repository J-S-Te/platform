package application

import (
	"fmt"
	"strings"
	"time"
)

// 交接机器协议：平台与各业务系统之间的责任发现、冻结与逐项转移约定。
// 业务数据仍由各子系统负责，平台只保存交接控制记录与真实回执。

// 系统适用性：未安装 = 不适用（不阻断）；已安装但不可达/不兼容/授权拒绝 = 阻断。
const (
	HandoverSystemApplicable    = "APPLICABLE"
	HandoverSystemNotInstalled  = "NOT_INSTALLED"
	HandoverSystemUnreachable   = "UNREACHABLE"
	HandoverSystemIncompatible  = "INCOMPATIBLE"
	HandoverSystemLicenseDenied = "LICENSE_DENIED"
)

// 交接项状态。
const (
	HandoverItemPending     = "PENDING"
	HandoverItemTransferred = "TRANSFERRED"
	HandoverItemFailed      = "FAILED"
	HandoverItemSkipped     = "SKIPPED"
)

// 转移回执状态。APPLIED/ALREADY_APPLIED 为终态成功，REJECTED 为终态失败，
// RETRYABLE 表示可退避重试（超时后必须先查回执再重发）。
const (
	HandoverReceiptApplied        = "APPLIED"
	HandoverReceiptAlreadyApplied = "ALREADY_APPLIED"
	HandoverReceiptRejected       = "REJECTED"
	HandoverReceiptRetryable      = "RETRYABLE"
)

// 交接批次状态。
const (
	HandoverBatchDiscovering  = "DISCOVERING"
	HandoverBatchFrozen       = "FROZEN"
	HandoverBatchTransferring = "TRANSFERRING"
	HandoverBatchCompleted    = "COMPLETED"
	HandoverBatchCancelled    = "CANCELLED"
	HandoverBatchBlocked      = "BLOCKED"
)

// HandoverBatch 是一次离职交接的控制记录（与人员异动请求一一对应）。
type HandoverBatch struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	RequestID         string     `json:"request_id"`
	UserID            string     `json:"user_id"`
	Status            string     `json:"status"`
	Version           uint64     `json:"version"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// HandoverSystemState 是控制面记录的单个系统交接状态。
type HandoverSystemState struct {
	System     string     `json:"system"`
	Capability string     `json:"capability"`
	Frozen     bool       `json:"frozen"`
	FrozenAt   *time.Time `json:"frozen_at,omitempty"`
}

// HandoverFreezeRequest 要求业务系统持久化该人员的责任分配冻结，再返回权威清单。
type HandoverFreezeRequest struct {
	TenantID    string    `json:"tenant_id"`
	BatchID     string    `json:"batch_id"`
	RequestID   string    `json:"request_id"`
	UserID      string    `json:"user_id"`
	RequestedAt time.Time `json:"requested_at"`
}

// HandoverFreezeResponse 是冻结结果；冻结必须先于任何责任清单返回。
type HandoverFreezeResponse struct {
	System              string     `json:"system"`
	Frozen              bool       `json:"frozen"`
	FrozenAt            *time.Time `json:"frozen_at,omitempty"`
	ResponsibilityCount int        `json:"responsibility_count"`
}

// HandoverResponsibility 是业务系统返回的权威责任项；Version 用于承接人变更的乐观并发。
type HandoverResponsibility struct {
	System       string    `json:"system,omitempty"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	ResourceName string    `json:"resource_name,omitempty"`
	OwnerUserID  string    `json:"owner_user_id"`
	Category     string    `json:"category,omitempty"`
	Version      uint64    `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// HandoverResponsibilityPage 是分页后的责任清单；NextCursor 为空表示已到末页。
type HandoverResponsibilityPage struct {
	System     string                   `json:"system"`
	Items      []HandoverResponsibility `json:"items"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// HandoverTransferRequest 是平台派发的逐项转移；幂等键与预期版本必须原样带到业务系统。
type HandoverTransferRequest struct {
	TenantID        string    `json:"tenant_id"`
	BatchID         string    `json:"batch_id"`
	ItemID          string    `json:"item_id"`
	RequestID       string    `json:"request_id"`
	System          string    `json:"system"`
	ResourceType    string    `json:"resource_type"`
	ResourceID      string    `json:"resource_id"`
	FromUserID      string    `json:"from_user_id"`
	ToUserID        string    `json:"to_user_id"`
	ExpectedVersion uint64    `json:"expected_version"`
	IdempotencyKey  string    `json:"idempotency_key"`
	LeaseID         string    `json:"lease_id"`
	Deadline        time.Time `json:"deadline"`
}

// HandoverTransferReceipt 是业务系统在本地事务完成后返回的真实回执。
type HandoverTransferReceipt struct {
	System         string    `json:"system"`
	ItemID         string    `json:"item_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Status         string    `json:"status"`
	AppliedVersion uint64    `json:"applied_version"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	EvidenceRef    string    `json:"evidence_ref,omitempty"`
	ReceivedAt     time.Time `json:"received_at"`
}

// HandoverSystemDisposition 判定单个系统对本次交接的影响。
func HandoverSystemDisposition(capability string) (blocking bool, notApplicable bool) {
	switch capability {
	case HandoverSystemApplicable:
		return false, false
	case HandoverSystemNotInstalled:
		// 未安装系统依据控制面事实标为不适用，不能当成"无责任"而静默跳过其余系统。
		return false, true
	default:
		// 已安装但不可达、接口不兼容或商业授权拒绝：必须阻断，不得降级为人工标完成。
		return true, false
	}
}

// HandoverIdempotencyKey 为逐项转移生成稳定幂等键；重试与重启后必须复用同一个键。
func HandoverIdempotencyKey(batchID, itemID string) string {
	return fmt.Sprintf("handover:%s:%s", strings.TrimSpace(batchID), strings.TrimSpace(itemID))
}

// HandoverRetryBackoff 返回下一次重试的等待时长：从 5 秒起步指数退避，上限 10 分钟。
func HandoverRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 5 * time.Second
	}
	const base, cap = 5 * time.Second, 10 * time.Minute
	backoff := base
	for i := 0; i < attempt && backoff < cap; i++ {
		backoff *= 2
	}
	if backoff > cap {
		return cap
	}
	return backoff
}

// NextHandoverRetryAt 计算下一次重试时间。
func NextHandoverRetryAt(attempt int, now time.Time) time.Time {
	return now.UTC().Add(HandoverRetryBackoff(attempt))
}

// HandoverLeaseExpired 判断领取租约是否已过期；过期前不得并发派发同一项。
func HandoverLeaseExpired(leaseDeadline, now time.Time) bool {
	return !leaseDeadline.After(now.UTC())
}

// HandoverReceiptTerminal 判定回执是否为终态。
func HandoverReceiptTerminal(status string) (terminal bool, success bool) {
	switch status {
	case HandoverReceiptApplied, HandoverReceiptAlreadyApplied:
		return true, true
	case HandoverReceiptRejected:
		return true, false
	default:
		return false, false
	}
}

// HandoverBatchCompletable 判断交接批次能否进入排期：存在阻断系统或未结项一律不允许。
func HandoverBatchCompletable(items []HandoverItem, systems []HandoverSystemState) bool {
	for _, system := range systems {
		if blocking, _ := HandoverSystemDisposition(system.Capability); blocking {
			return false
		}
	}
	for _, item := range items {
		switch item.Status {
		case HandoverItemTransferred, HandoverItemSkipped:
		default:
			// PENDING/FAILED/未知状态都视为未结项。
			return false
		}
	}
	return true
}
