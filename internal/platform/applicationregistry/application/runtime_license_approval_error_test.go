package application

import (
	"fmt"
	"testing"
)

// 授权实例未初始化时必须映射到用户可读的 LICENSE_NOT_INITIALIZED，
// 否则接入失败只会落到与授权无关的通用“部署 Agent 执行失败”。
// V1 审批路径下线后这条映射仍是 V2 诊断链的一部分。
func TestRuntimeApprovalErrorCodeClassifiesUninitializedLicenseInstance(t *testing.T) {
	if got := RuntimeLicenseApprovalErrorCode(ErrRuntimeLicenseNotInitialized); got != "LICENSE_NOT_INITIALIZED" {
		t.Fatalf("code = %q, want LICENSE_NOT_INITIALIZED", got)
	}
	wrapped := fmt.Errorf("deliver local runtime license: %w", ErrRuntimeLicenseNotInitialized)
	if got := RuntimeLicenseApprovalErrorCode(wrapped); got != "LICENSE_NOT_INITIALIZED" {
		t.Fatalf("wrapped code = %q, want LICENSE_NOT_INITIALIZED", got)
	}
}
