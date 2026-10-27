package infrastructure

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// initializeInheritedRuntimeExtensions 用途：升级时从审核模板补建新增的继承式 Worker 配置。
// 参数：input 为受控更新输入，仅用于核对目标；调用方必须持有发布锁。
// 返回值：缺失源配置、模板或不安全路径返回错误；不重建应用主配置和既有秘密。
func (target *productionComposeTarget) initializeInheritedRuntimeExtensions(input application.SubsystemProvisioningInput) error {
	if !target.matches(input.ApplicationCode, input.Environment) {
		return application.ErrValidation
	}
	root, err := canonicalProductionDeployRoot(target.config.DeployRoot)
	if err != nil {
		return err
	}
	files := target.selectedRuntimeFiles()
	for _, file := range files {
		if file.InheritFrom == "" || len(file.InheritKeys) == 0 || len(file.RequiredExistingKeys) > 0 {
			continue
		}
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file.Path)))
		if !errors.Is(err, os.ErrNotExist) {
			continue
		}
		sourceFound := false
		for _, source := range files {
			if source.Path == file.InheritFrom {
				if err := ensureProductionRuntimeFile(root, source, false); err != nil {
					return err
				}
				sourceFound = true
				break
			}
		}
		if !sourceFound {
			return provisioningError("production runtime inheritance source unavailable")
		}
		if err := ensureProductionRuntimeFile(root, file, true); err != nil {
			return err
		}
	}
	return nil
}
