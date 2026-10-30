package infrastructure

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessProofsUseRuntimeDirectoryOwner 验证Agent生成及恢复证明可被部署用户私有备份。
// 参数：t为隔离root上下文；全部文件在本测试临时目录中，默认非root跳过。
// 返回值：无，正负证明的属主、权限或内容保持不符合要求时失败。
func TestAccessProofsUseRuntimeDirectoryOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root execution for Agent/deploy ownership")
	}
	for _, negative := range []bool{true, false} {
		t.Run(map[bool]string{true: "negative", false: "success"}[negative], func(t *testing.T) {
			root := t.TempDir()
			runtime := filepath.Join(root, "runtime")
			if err := os.Mkdir(runtime, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(runtime, 1000, 1000); err != nil {
				t.Fatal(err)
			}
			receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "operation-1",
				ApplicationCode: "settlement", Environment: "dev", Generation: 1, ManifestChecksum: "manifest-1"}
			var path string
			var err error
			if negative {
				path, err = accessOutcomePath(root, receipt)
				if err != nil {
					t.Fatal(err)
				}
				err = writeAccessDeploymentOutcome(root, application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
					State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired, RecordedAt: time.Now().UTC()})
			} else {
				path, err = accessReceiptPath(root, receipt)
				if err != nil {
					t.Fatal(err)
				}
				err = writeAccessDeploymentReceipt(root, application.SubsystemProvisioningInput{TenantID: receipt.TenantID,
					ApplicationCode: receipt.ApplicationCode, Environment: receipt.Environment, ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, migrate := range []bool{false, true} {
				if migrate {
					before, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					for _, p := range []string{filepath.Dir(path), path} {
						if err = os.Chown(p, 0, 0); err != nil {
							t.Fatal(err)
						}
					}
					if negative {
						_, _, err = readAccessDeploymentOutcome(root, receipt)
					} else {
						_, err = readAccessDeploymentReceipt(root, receipt)
					}
					if err != nil {
						t.Fatal(err)
					}
					after, err := os.ReadFile(path)
					if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
						t.Fatal("ownership repair changed proof content", err)
					}
				}
				for _, p := range []string{filepath.Dir(path), path} {
					info, err := os.Stat(p)
					if err != nil {
						t.Fatal(err)
					}
					owner := info.Sys().(*syscall.Stat_t)
					mode := os.FileMode(0600)
					if info.IsDir() {
						mode = 0700
					}
					if owner.Uid != 1000 || owner.Gid != 1000 || info.Mode().Perm() != mode {
						t.Fatalf("proof ownership: uid=%d gid=%d mode=%o", owner.Uid, owner.Gid, info.Mode().Perm())
					}
				}
			}
		})
	}
}
