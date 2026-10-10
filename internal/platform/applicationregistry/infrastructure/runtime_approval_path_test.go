package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeApprovalDirectoryIsAuthoritative(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	root := target.config.DeployRoot
	name := "runtime-license-contract_management-prod.json"
	legacy := filepath.Join(root, name)
	path, err := productionRuntimeApprovalPath(root, name)
	if err != nil || path != legacy {
		t.Fatal("legacy compatibility unavailable", path, err)
	}
	raw, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "runtime-approvals")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path, err = productionRuntimeApprovalPath(root, name)
	if err != nil || path != filepath.Join(dir, name) {
		t.Fatal("new directory not authoritative", path, err)
	}
	for _, content := range []string{"", "not-json"} {
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := target.writeRuntimeLicenseCredentials(input); err == nil {
			t.Fatal("delivery fell back to legacy approval")
		}
		if err := target.retireRuntimeLicenseContainers(context.Background(), input); err == nil {
			t.Fatal("retirement fell back to legacy approval")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("obsolete-invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal("new delivery approval rejected", err)
	}
	if err := target.retireRuntimeLicenseContainers(context.Background(), input); err != nil {
		t.Fatal("new retirement approval rejected", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(legacy, path); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeLicenseCredentials(input); err == nil {
		t.Fatal("symlink release accepted by delivery")
	}
	if err := target.retireRuntimeLicenseContainers(context.Background(), input); err == nil {
		t.Fatal("symlink release accepted by retirement")
	}
}

func TestRuntimeApprovalPathRejectsUnsafeDirectoryAndNames(t *testing.T) {
	for _, kind := range []string{"symlink", "file", "writable"} {
		t.Run(kind, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "runtime-approvals")
			switch kind {
			case "symlink":
				err = os.Symlink(t.TempDir(), dir)
			case "file":
				err = os.WriteFile(dir, []byte("invalid"), 0600)
			case "writable":
				err = os.Mkdir(dir, 0700)
				if err == nil {
					err = os.Chmod(dir, 0777)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := productionRuntimeApprovalPath(root, "license-evidence.json"); err == nil {
				t.Fatal("unsafe directory accepted")
			}
		})
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../license-evidence.json", "arbitrary.json", "runtime-license-contract_management-dev.json", "commercial-license.jws"} {
		if _, err := productionRuntimeApprovalPath(root, name); err == nil {
			t.Fatal("unsupported approval name accepted", name)
		}
	}
}

func TestEvidenceApprovalMountedDirectoryCannotFallBack(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "runtime-approvals")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"license-evidence.json", "license-migration-evidence.json"} {
		raw := []byte(`{"version":1,"project":"production","installation_boundary":true,"services":[],"excluded_services":["platform-api"]}`)
		if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		path, err := productionRuntimeApprovalPath(root, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"missing", "invalid", "symlink", "valid"} {
			if state == "invalid" {
				err = os.WriteFile(path, []byte("invalid"), 0600)
			}
			if state == "symlink" {
				if err = os.Remove(path); err == nil {
					err = os.Symlink(filepath.Join(root, name), path)
				}
			}
			if state == "valid" {
				if err = os.Remove(path); err == nil {
					err = os.WriteFile(path, raw, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "license-evidence.json" {
				_, err = loadInstallationLicenseEvidenceApproval(path, "production", nil)
			} else {
				_, err = loadMigrationInstallationEvidenceApproval(path, "production", nil)
			}
			if (err == nil) != (state == "valid") {
				t.Fatalf("%s %s approval fallback or rejection: %v", name, state, err)
			}
			err = nil
		}
	}
}
