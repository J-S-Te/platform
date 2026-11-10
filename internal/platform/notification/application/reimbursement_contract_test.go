package application

import "testing"

// TestReimbursementNotificationContract 用途：核验报销真实事件代码与ULID映射满足平台摄取契约。
// 参数：t为生产参数校验测试上下文。
// 返回值：无；正式DTO代码、深链或稳定幂等键被拒时失败。
func TestReimbursementNotificationContract(t *testing.T) {
	for _, kind := range []string{"APPROVAL_ASSIGNED", "APPROVAL_REMINDER", "INVOICE_MISMATCH", "INVOICE_CORRECTION_REQUESTED", "INVOICE_RECONFIRM_REQUIRED", "INVOICE_FINANCE_REVIEW_REQUIRED"} {
		input := validIngestInput()
		input.SourceApplication = "reimbursement"
		input.SourceEnvironment = "prod"
		input.Event.EventID = "REIMBURSEMENT_01H00000000000000000000000"
		input.Event.EventType = "REIMBURSEMENT_" + kind
		input.Event.TargetURL = "/reimbursement/claims?claim_id=claim"
		input.Event.IdempotencyKey = "01H00000000000000000000000"
		input.Event.ReferenceType = "reimbursement_claim"
		input.Event.ReferenceID = "claim"
		if err := validateIngestInput(input); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}
