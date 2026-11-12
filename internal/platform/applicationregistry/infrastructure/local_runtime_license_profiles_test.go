package infrastructure

import "testing"

// TestLocalLicenseComposeProfilesIncludeReimbursement 锁定本地许可运行态解析镜像所需的 compose profile 全集。
// 报销服务（api/worker/migrate/catalog-sync）只在这些 profile 下会被 docker compose config 展开；
// 缺任一 profile 都会让首次接入与 retry 重复报 local service/migration image missing。
// 参数：t为测试上下文。
// 返回值：profile 缺失或重复时失败。
func TestLocalLicenseComposeProfilesIncludeReimbursement(t *testing.T) {
	seen := map[string]bool{}
	for _, profile := range localLicenseComposeProfiles {
		if seen[profile] {
			t.Fatalf("duplicate compose profile %q", profile)
		}
		seen[profile] = true
	}
	for _, required := range []string{"reimbursement", "reimbursement-release"} {
		if !seen[required] {
			t.Fatalf("missing required compose profile %q", required)
		}
	}
	// 集成子系统重建路径与许可镜像解析路径必须共用同一份 profile 标志。
	args := localLicenseComposeProfileArgs()
	if len(args) != len(localLicenseComposeProfiles)*2 {
		t.Fatalf("profile args = %v; want %d entries", args, len(localLicenseComposeProfiles)*2)
	}
	flags := map[string]bool{}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "--profile" {
			t.Fatalf("unexpected compose flag %q", args[i])
		}
		flags[args[i+1]] = true
	}
	for _, required := range []string{"reimbursement", "reimbursement-release"} {
		if !flags[required] {
			t.Fatalf("profile args missing %q: %v", required, args)
		}
	}
}
