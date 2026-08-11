package domain

const (
	PersonnelChangePromotion       = "PROMOTION"
	PersonnelChangeDemotion        = "DEMOTION"
	PersonnelChangeTransfer        = "TRANSFER"
	PersonnelChangeTermination     = "TERMINATION"
	PersonnelChangeRehire          = "REHIRE"
	PersonnelChangeDraft           = "DRAFT"
	PersonnelChangePendingApproval = "PENDING_APPROVAL"
	PersonnelChangePendingHandover = "PENDING_HANDOVER"
	PersonnelChangeScheduled       = "SCHEDULED"
	PersonnelChangeExecuted        = "EXECUTED"
	PersonnelChangeRejected        = "REJECTED"
	PersonnelChangeCancelled       = "CANCELLED"
)

// CanTransitionPersonnelChange 是审批、交接和定时执行共用的生命周期闸门；终态请求不可重开。
func CanTransitionPersonnelChange(from, to string) bool {
	// 拒绝自转换，避免重复提交被误当成一次有效状态变更。
	if from == to {
		return false
	}
	switch from {
	case PersonnelChangeDraft:
		// 草稿只能提交审批或取消，不能跳过审批直接执行。
		return to == PersonnelChangePendingApproval || to == PersonnelChangeCancelled
	case PersonnelChangePendingApproval:
		// 审批后可进入交接或排期，也可被驳回或取消。
		return to == PersonnelChangePendingHandover || to == PersonnelChangeScheduled || to == PersonnelChangeRejected || to == PersonnelChangeCancelled
	case PersonnelChangePendingHandover:
		// 交接阶段只能排期或取消，不能回退到审批阶段。
		return to == PersonnelChangeScheduled || to == PersonnelChangeCancelled
	case PersonnelChangeScheduled:
		// 排期请求只能在到期后执行，或在执行前取消。
		return to == PersonnelChangeExecuted || to == PersonnelChangeCancelled
	}
	// 驳回、取消和执行均为终态，其他未知状态默认拒绝流转。
	return false
}
