package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProductionLocalPortalOmitsCRMCrossWrite 验证本地门户不会再修改已安装CRM配置或启动邀请补偿。
// 参数：t为测试上下文，配置均写入私有临时目录。
// 返回值：无；本地模式仍触及CRM或默认CRM被改变时失败。
func TestProductionLocalPortalOmitsCRMCrossWrite(t *testing.T) {
	for _, mode := range []string{"", "crm", "local"} {
		t.Run("mode="+mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			profile, err := decodeProductionSubsystemProfile(platformModuleRoot(t), filepath.Join(platformModuleRoot(t), "deploy/production/subsystems.d/customer_portal-prod.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "runtime/portal.env"), []byte("PORTAL_CUSTOMER_DIRECTORY_MODE="+mode+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			dependency := ""
			for _, file := range profile.Manifest.Runtime.Files {
				if file.Path == "runtime/customer.env" {
					for _, key := range file.RequiredExistingKeys {
						dependency += key + "=synthetic-ready\n"
					}
				}
			}
			if err := os.WriteFile(filepath.Join(root, "runtime/customer.env"), []byte(dependency), 0600); err != nil {
				t.Fatal(err)
			}
			target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}, enabledServices: map[string]bool{"customer-api": true}}
			found := false
			for _, file := range target.selectedRuntimeFiles() {
				found = found || file.Path == "runtime/customer.env"
			}
			if found != (mode != "local") {
				t.Fatalf("unexpected CRM runtime participation for %q", mode)
			}
			if containsString(target.selectedRuntimeServices(), "portal-invite-compensation-worker") != (mode != "local") {
				t.Fatal("consumer selection diverges from runtime writes")
			}
			if !containsString(target.selectedRuntimeServices(), "portal-report-worker") {
				t.Fatal("local PDF ingest worker omitted")
			}
		})
	}
}

// TestProductionPortalModeRejectsUnsafe 验证客户来源策略不能由不安全文件或未知值触发。
// 参数：t为测试上下文，仅使用本轮临时配置。
// 返回值：无；未知值、公开文件或符号链接被接受时失败。
func TestProductionPortalModeRejectsUnsafe(t *testing.T) {
	for _, state := range []string{"invalid", "public", "symlink"} {
		t.Run(state, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "runtime/portal.env")
			content := "PORTAL_CUSTOMER_DIRECTORY_MODE=local\n"
			mode := os.FileMode(0600)
			if state == "invalid" {
				content = "PORTAL_CUSTOMER_DIRECTORY_MODE=automatic\n"
			}
			if state == "public" {
				mode = 0644
			}
			if err := os.WriteFile(path, []byte(content), mode); err != nil {
				t.Fatal(err)
			}
			if state == "symlink" {
				if err := os.Rename(path, path+".source"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".source", path); err != nil {
					t.Fatal(err)
				}
			}
			profile := productionSubsystemProfile{Manifest: productionSubsystemManifest{Application: productionSubsystemApplicationManifest{Code: "customer_portal"}}}
			target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}
			if _, err := target.portalLocalDirectory(); err == nil {
				t.Fatal("unsafe directory mode accepted")
			}
		})
	}
}
