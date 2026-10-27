package infrastructure

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestRuntimeLicenseCredentialsUseRuntimeDirectoryOwner 用途：验证生成及重写凭据的部署属主与私有权限。
// 参数：t 为隔离测试上下文；测试仅操作临时目录，需要隔离的 root 执行环境。
// 返回值：无；属主或权限不符合要求时报告测试失败。
func TestRuntimeLicenseCredentialsUseRuntimeDirectoryOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root execution to reproduce Agent/deploy UID boundary")
	}
	root := t.TempDir()
	if err := os.Chown(root, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "license-contract-api.env")
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			if err := os.Chown(path, 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeIsolatedRuntimeLicenseEnvironment(path, map[string]string{"COMMERCIAL_LICENSE_CLIENT_SECRET": "synthetic-test-secret"}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		owner := info.Sys().(*syscall.Stat_t)
		if owner.Uid != 1000 || owner.Gid != 1000 || info.Mode().Perm() != 0600 {
			t.Fatalf("credentials not privately owned by runtime operator: uid=%d gid=%d mode=%o", owner.Uid, owner.Gid, info.Mode().Perm())
		}
	}
}
