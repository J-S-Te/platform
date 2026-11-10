package filetaskhttp

import "testing"

// TestReimbursementUploadPolicies 验证报销文件用途只允许报销应用和明确媒体类型。
// 参数：t 为测试上下文。
// 返回值：无；用途缺失或跨应用可用时失败。
func TestReimbursementUploadPolicies(t *testing.T) {
	for _, purpose := range []string{"reimbursement.invoice.original", "reimbursement.payment.voucher", "reimbursement.claim.evidence", "reimbursement.archive.package", "reimbursement.export.result"} {
		p, ok := uploadPolicies[purpose]
		if !ok || p.Namespace != "reimbursement" || p.MaxBytes > 20<<20 || !validPurposeApplication("reimbursement", p.Namespace) || validPurposeApplication("project_management", p.Namespace) {
			t.Fatalf("policy %s=%+v", purpose, p)
		}
	}
	invoice := uploadPolicies["reimbursement.invoice.original"]
	if _, ok := invoice.Media["application/ofd"]; !ok {
		t.Fatal("OFD missing")
	}
	for _, media := range []string{"image/png", "image/jpeg", "application/vnd.ofd"} {
		if _, ok := invoice.Media[media]; !ok {
			t.Fatalf("人工核验原件缺少支持格式 %s", media)
		}
	}
}
