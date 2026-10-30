package infrastructure

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProductionProjectMigrationBuildBinding 保证迁移进程也接收与项目镜像绑定的构建身份。
// 参数：t为测试上下文，读取正式受控部署编排。
// 返回值：无；项目配置启用Temporal而迁移缺失构建绑定时失败。
func TestProductionProjectMigrationBuildBinding(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(platformModuleRoot(t), "deploy", "production", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project-migrate", "project-api", "project-sla-notifier"} {
		service := compose.Services[name]
		if !projectBackgroundReleaseBinding(service.Image, "PROJECT_IMAGE") ||
			!projectBackgroundReleaseBinding(service.Environment["PROJECT_BACKGROUND_BUILD_ID"], "PROJECT_BACKGROUND_BUILD_ID") {
			t.Errorf("%s must receive the immutable project image and its background build ID", name)
		}
	}
}
