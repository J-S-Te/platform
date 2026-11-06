package infrastructure

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// TestPortalWorkerInheritsCustomerAuthority 验证报告Worker与API保持同一来源，旧配置默认CRM。
// 参数：t为测试上下文，仅使用独立临时运行文件。
// 返回值：无；模式遗漏、非法来源接受或旧安装被阻断时失败。
func TestPortalWorkerInheritsCustomerAuthority(t *testing.T) {
	for _, mode := range []string{"", "crm", "local", "automatic"} {
		t.Run("mode="+mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "runtime/portal.env"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			source := map[string]string{"PORTAL_REPORT_CONTENT_KEY_BASE64": base64.StdEncoding.EncodeToString(make([]byte, 32)), "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))}
			if mode != "" {
				source["PORTAL_CUSTOMER_DIRECTORY_MODE"] = mode
			}
			file := productionSubsystemRuntimeFileManifest{InheritFrom: "runtime/portal.env", InheritKeys: []string{"PORTAL_CUSTOMER_DIRECTORY_MODE"}}
			values := map[string]string{}
			err = inheritProductionRuntimeValues(root, file, map[string]map[string]string{file.InheritFrom: source}, values)
			if mode == "automatic" {
				if err == nil {
					t.Fatal("unknown authority accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := mode
			if want == "" {
				want = "crm"
			}
			if values["PORTAL_CUSTOMER_DIRECTORY_MODE"] != want {
				t.Fatal("worker authority differs from API")
			}
		})
	}
}
