package infrastructure

import (
	"os"
	"path/filepath"
	"strings"
)

// portalLocalDirectory 检查已冻结的门户客户来源，默认保持既有CRM模式。
// 参数：无；只读取当前部署根中门户的私有运行文件。
// 返回值：本地模式及错误；非法模式或不安全文件禁止部署。
func (target *productionComposeTarget) portalLocalDirectory() (bool, error) {
	if target.config.Profile.Manifest.Application.Code != "customer_portal" {
		return false, nil
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/portal.env")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if err != nil || resolveErr != nil || resolved != path || !info.Mode().IsRegular() ||
		!pathWithinRoot(target.config.DeployRoot, resolved) || info.Mode().Perm()&0077 != 0 {
		return false, provisioningError("portal directory runtime file is unsafe")
	}
	values, err := readEnvironmentValues(path)
	if err != nil {
		return false, provisioningError("portal directory runtime cannot be read")
	}
	switch strings.TrimSpace(values["PORTAL_CUSTOMER_DIRECTORY_MODE"]) {
	case "", "crm":
		return false, nil
	case "local":
		return true, nil
	default:
		return false, provisioningError("portal customer directory mode is invalid")
	}
}
