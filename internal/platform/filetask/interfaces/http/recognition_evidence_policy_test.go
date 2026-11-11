package filetaskhttp

import "testing"

// TestRecognitionEvidencePolicy 验证确认凭证仅结算应用可上传且受固定边界约束。
// 参数：t为文件用途策略测试上下文。
// 返回值：无；用途缺失、跨应用或媒体/大小越界时失败。
func TestRecognitionEvidencePolicy(t *testing.T) {
	item, ok := uploadPolicies["settlement.recognition.evidence"]
	if !ok || item.Namespace != "settlement" || item.Purpose != "recognition-evidence" ||
		item.MaxBytes != 10<<20 || item.Temporary || len(item.Media) != 3 ||
		item.Classification != "CONFIDENTIAL" {
		t.Fatalf("missing or unsafe policy: %+v", item)
	}
	for _, media := range []string{"application/pdf", "image/jpeg", "image/png"} {
		if _, ok := item.Media[media]; !ok {
			t.Fatalf("missing accepted evidence media %s", media)
		}
	}
	if !validPurposeApplication("settlement", item.Namespace) ||
		validPurposeApplication("reimbursement", item.Namespace) ||
		validPurposeApplication("data_analysis", item.Namespace) {
		t.Fatal("evidence namespace boundary failed")
	}
}
