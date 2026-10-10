package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
)

// A mounted approval directory is authoritative even when empty. Falling back
// from a missing or invalid new approval could revive an obsolete release.
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
