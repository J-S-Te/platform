package infrastructure

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestProductionServiceBindingsHaveUpdateCredentialRequirements 验证清单声明的
// service.<purpose> 绑定都能由受控更新重新交付：否则首次接入成功后，任何更新都会
// 因缺少同用途凭据而在生成运行文件时失败（BIZ-XAPI-02 / BIZ-XWIRE-06 的部署侧一致性）。
// 参数：t 为测试上下文。
// 返回值：无；绑定用途缺少更新需求时测试失败。
func TestProductionServiceBindingsHaveUpdateCredentialRequirements(t *testing.T) {
	root := platformModuleRoot(t)
	profiles, _, err := loadProductionSubsystemProfiles(
		filepath.Join(root, "deploy", "production"),
		filepath.Join(root, "deploy", "production", "subsystems.d"),
	)
	if err != nil {
		t.Fatalf("load repository profiles: %v", err)
	}
	for _, profile := range profiles {
		applicationCode := profile.Manifest.Application.Code
		required := make(map[string]bool)
		for _, requirement := range application.SubsystemUpdateServiceCredentialRequirements(applicationCode) {
			required[requirement.Purpose] = true
		}
		for _, runtimeFile := range profile.Manifest.Runtime.Files {
			for key, source := range runtimeFile.Bindings {
				parts := strings.Split(source, ".")
				if len(parts) != 3 || parts[0] != "service" || parts[2] != "client_id" {
					// related_service.* 由关联应用凭据解析，不进入本应用更新需求。
					continue
				}
				if !required[parts[1]] {
					t.Fatalf("%s: binding %s references purpose %s without an update credential requirement", applicationCode, key, parts[1])
				}
			}
		}
	}
}
