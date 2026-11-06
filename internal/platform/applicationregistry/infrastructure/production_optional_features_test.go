package infrastructure

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const reviewedEmbedFeature = `
optional_features:
  - name: dashboard_embed
    runtime_file: runtime/data-analysis.env
    enabled_key: DASHBOARD_EMBED_ENABLED
    default_enabled: true
    required_existing_keys: [METABASE_EMBEDDING_SECRET]
    runtime_services: [data-analysis-metabase]
    initialization_services: [data-analysis-metabase-init]
`

// optionalFeatureFixture 加载实际数析清单并附加审核过的嵌入可选策略。
// 参数：t为测试上下文，flag为临时运行配置中的显式模式。
// 返回值：临时目标及运行文件；解析失败立即终止测试。
func optionalFeatureFixture(t *testing.T, flag string) (*productionComposeTarget, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(platformModuleRoot(t), "deploy/production/subsystems.d/data-analysis-prod.yaml")
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// 避免实际清单随后纳入相同策略时重复声明。
	content = []byte(strings.Split(string(content), "\noptional_features:")[0] + reviewedEmbedFeature)
	profilePath := filepath.Join(root, "analysis.yaml")
	if err := os.WriteFile(profilePath, content, 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := decodeProductionSubsystemProfile(root, profilePath)
	if err != nil {
		t.Fatalf("reviewed optional policy must load: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(root, "runtime/data-analysis.env")
	if err := os.WriteFile(runtime, []byte("DASHBOARD_EMBED_ENABLED="+flag+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}, runtime
}

// TestProductionOptionalEmbedDoesNotSelectMetabase 验证原生模式不再要求嵌入进程。
// 参数：t为测试上下文。
// 返回值：无；错误的选择或策略解析将使测试失败。
func TestProductionOptionalEmbedDoesNotSelectMetabase(t *testing.T) {
	target, _ := optionalFeatureFixture(t, "false")
	selected := target.selectedRuntimeServices()
	if slices.Contains(selected, "data-analysis-metabase") {
		t.Fatal("native analysis must not start Metabase")
	}
	for _, service := range []string{"data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"} {
		if !slices.Contains(selected, service) {
			t.Fatalf("native mode removed required consumer %s", service)
		}
	}
}

// TestProductionOptionalEmbedValidatesModeAndSecret 核验关闭、兼容默认和非法模式的材料边界。
// 参数：t为测试上下文。
// 返回值：无；未启用仍要求密钥或启用却漏检密钥时失败。
func TestProductionOptionalEmbedValidatesModeAndSecret(t *testing.T) {
	for _, row := range []struct {
		name, flag, secret string
		wantError          bool
	}{
		{"native", "false", "", false},
		{"enabled missing secret", "true", "", true},
		{"legacy default", "", "", true},
		{"enabled configured", "true", "synthetic-embedding-key", false},
		{"invalid flag", "sometimes", "synthetic-embedding-key", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			target, path := optionalFeatureFixture(t, row.flag)
			if row.secret != "" {
				if err := updateProductionSubsystemEnvironment(path, map[string]string{"METABASE_EMBEDDING_SECRET": row.secret}); err != nil {
					t.Fatal(err)
				}
			}
			if err := target.validateOptionalFeatureRuntime(); (err != nil) != row.wantError {
				t.Fatalf("unexpected feature validation result: error=%t", err != nil)
			}
			init := target.selectOptionalServices(target.config.Profile.Manifest.Compose.InitializationServices, true)
			if (row.flag == "false") == slices.Contains(init, "data-analysis-metabase-init") {
				t.Fatal("feature initialization selection does not match mode")
			}
		})
	}
}

// TestProductionOptionalEmbedCannotRemoveBusinessConsumer 防止可选策略借配置省略业务许可消费者。
// 参数：t为测试上下文。
// 返回值：无；API或Worker可被关闭时失败。
func TestProductionOptionalEmbedCannotRemoveBusinessConsumer(t *testing.T) {
	for _, service := range []string{"data-analysis-api", "data-analysis-alert-worker", "unknown"} {
		t.Run(service, func(t *testing.T) {
			target, _ := optionalFeatureFixture(t, "false")
			target.config.Profile.Manifest.OptionalFeatures[0].RuntimeServices = []string{service}
			if err := validateOptionalFeaturePolicies(&target.config.Profile.Manifest); err == nil {
				t.Fatal("unreviewed consumer accepted as optional infrastructure")
			}
		})
	}
}

// TestProductionOptionalEmbedPreservesModeOnRuntimeWrites 验证首次配置与重试不覆盖已明确关闭的模式。
// 参数：t为测试上下文。
// 返回值：无；更新或配置重投改变模式时失败。
func TestProductionOptionalEmbedPreservesModeOnRuntimeWrites(t *testing.T) {
	target, path := optionalFeatureFixture(t, "false")
	file := &target.config.Profile.Manifest.Runtime.Files[0]
	file.Bindings, file.GeneratedKeys = nil, nil
	file.Values = map[string]string{"DASHBOARD_EMBED_ENABLED": "true"}
	input := application.SubsystemProvisioningInput{ApplicationCode: "data_analysis"}
	for _, write := range []func(application.SubsystemProvisioningInput) error{
		target.writeRuntimeConfiguration, target.writeRuntimeFixedValues,
	} {
		if err := write(input); err != nil {
			t.Fatal(err)
		}
		values, err := readEnvironmentValues(path)
		if err != nil {
			t.Fatal(err)
		}
		if values["DASHBOARD_EMBED_ENABLED"] != "false" {
			t.Fatal("runtime write changed selected native mode")
		}
	}
}

// TestProductionOptionalEmbedRejectsUnsafeRuntime 防止可选模式读取绕过私有运行文件边界。
// 参数：t为测试上下文。
// 返回值：无；符号链接被接受时失败。
func TestProductionOptionalEmbedRejectsUnsafeRuntime(t *testing.T) {
	target, path := optionalFeatureFixture(t, "false")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".old", path); err != nil {
		t.Fatal(err)
	}
	if err := target.validateOptionalFeatureRuntime(); err == nil {
		t.Fatal("symlink runtime accepted")
	}
}
