package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
)

// 挂载的批准目录即使为空也是权威的。从缺失或非法的新批准回退可能复活已废弃的发布。
func productionRuntimeApprovalPath(root, name string) (string, error) {
	allowed := map[string]bool{
		"license-evidence.json": true, "license-migration-evidence.json": true,
		"runtime-license-contract_management-prod.json":      true,
		"runtime-license-project_management-prod.json":       true,
		"runtime-license-customer_and_opportunity-prod.json": true,
		"runtime-license-customer_portal-prod.json":          true,
		"runtime-license-settlement-prod.json":               true,
		"runtime-license-data_analysis-prod.json":            true,
	}
	failure := provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: unsafe approval location")
	if !allowed[name] || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", failure
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return "", failure
	}
	dir := filepath.Join(root, "runtime-approvals")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(root, name), nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return "", failure
	}
	return filepath.Join(dir, name), nil
}
