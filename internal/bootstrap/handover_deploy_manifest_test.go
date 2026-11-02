package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 交接协议是跨仓库契约：平台出站（平台 .env）与四端入站验签清单/模板必须一致，
// 否则一边改了 issuer/audience/scope，另一边会静默 401。这里用部署文件做静态对账，
// 不连接任何环境。
const handoverDeployRoot = "../../deploy/production"

// handoverSubsystemExpectation 描述一个子系统入站验签键的期望值（系统编码 → 前缀）。
type handoverSubsystemExpectation struct {
	manifestPath string
	templatePath string
	// keys 是模板/清单必须成对出现且取值相同的键（不含前缀）。
	keys map[string]string
}

// handoverSubsystemExpectations 是当前决定的四端验签边界：合同/结算/项目切到平台
// Ed25519 公钥验签，CRM 沿用既有 machine auth（不需要新的验签键）。
func handoverSubsystemExpectations(audience string) map[string]handoverSubsystemExpectation {
	fixed := func(enabled string) map[string]string {
		return map[string]string{
			"ENABLED":                 enabled,
			"ISSUER":                  "basic-platform",
			"AUDIENCE":                audience,
			"PUBLIC_KEY_PATH":         "/app/data/keys/jwt-ed25519-public.pem",
			"CLIENT_ID":               "platform-handover",
			"CALLER_ENVIRONMENT_CODE": "prod",
		}
	}
	contract := fixed("true")
	contract["REQUIRE_BEARER"] = "true"
	contract["CALLER_APPLICATION_CODE"] = "platform"
	contract["REQUIRED_SCOPE"] = "handover.machine"
	settlement := fixed("true")
	settlement["APPLICATION_CODE"] = "platform"
	settlement["SCOPE"] = "handover.machine"
	project := fixed("true")
	project["CALLER_APPLICATION_CODE"] = "platform"
	project["REQUIRED_SCOPE"] = "handover.machine"
	return map[string]handoverSubsystemExpectation{
		"contract": {
			manifestPath: "subsystems.d/contract_management-prod.yaml",
			templatePath: "subsystem-templates/contract.env.example",
			keys:         contract,
		},
		"settlement": {
			manifestPath: "subsystems.d/settlement-prod.yaml",
			templatePath: "subsystem-templates/settlement.env.example",
			keys:         settlement,
		},
		"project": {
			manifestPath: "subsystems.d/project_management-prod.yaml",
			templatePath: "subsystem-templates/project.env.example",
			keys:         project,
		},
	}
}

// readEnvExample 解析 KEY=VALUE 形式的模板/.env 示例；注释与空行忽略。
func readEnvExample(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}

// readManifestValues 读取子系统生产清单中 runtime 文件注入的 env 值。
func readManifestValues(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest struct {
		Runtime struct {
			Files []struct {
				Values map[string]string `yaml:"values"`
			} `yaml:"files"`
		} `yaml:"runtime"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	values := map[string]string{}
	for _, file := range manifest.Runtime.Files {
		for key, value := range file.Values {
			values[key] = value
		}
	}
	if len(values) == 0 {
		t.Fatalf("%s carries no runtime env values", path)
	}
	return values
}

// TestPlatformHandoverEnvExampleMatchesProtocol 校验平台侧示例配置与协议约定一致：
// 关闭默认、清单含四系统、令牌标识齐全、不再出现静态机器令牌或令牌端点。
func TestPlatformHandoverEnvExampleMatchesProtocol(t *testing.T) {
	values := readEnvExample(t, filepath.Join(handoverDeployRoot, ".env.example"))
	if values["HANDOVER_ENABLED"] != "false" {
		t.Fatalf("HANDOVER_ENABLED = %q, want false by default", values["HANDOVER_ENABLED"])
	}
	for key, want := range map[string]string{
		"HANDOVER_MACHINE_CLIENT_ID":      "platform-handover",
		"HANDOVER_MACHINE_APPLICATION_ID": "platform",
		"HANDOVER_MACHINE_ENVIRONMENT_ID": "prod",
		"HANDOVER_MACHINE_SCOPE":          "handover.machine",
		"HANDOVER_ENVIRONMENT":            "prod",
	} {
		if got := values[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if _, found := values["HANDOVER_MACHINE_TOKEN"]; found {
		t.Fatal("HANDOVER_MACHINE_TOKEN must not be part of the platform configuration")
	}
	if _, found := values["HANDOVER_TOKEN_URL"]; found {
		t.Fatal("platform mints handover tokens itself; HANDOVER_TOKEN_URL must not be required")
	}
	for system, wantURL := range map[string]string{
		"contract":   "http://contract-api:8081",
		"crm":        "http://customer-api:8090/customer-opportunity",
		"project":    "http://project-api:8082",
		"settlement": "http://settlement-api:8085",
	} {
		entry := system + "=" + wantURL
		if !strings.Contains(values["HANDOVER_SYSTEM_URLS"], entry) {
			t.Fatalf("HANDOVER_SYSTEM_URLS = %q, want it to contain %q", values["HANDOVER_SYSTEM_URLS"], entry)
		}
	}
}

// TestSubsystemHandoverManifestsMatchTemplates 校验三端入站验签键在模板与生产清单中
// 成对出现且取值一致，audience 等于平台实际签发 audience（同一个全局值）。
func TestSubsystemHandoverManifestsMatchTemplates(t *testing.T) {
	platformEnv := readEnvExample(t, filepath.Join(handoverDeployRoot, ".env.example"))
	audience := platformEnv["AUTH_APPLICATION_JWT_AUDIENCE"]
	if audience == "" {
		t.Fatal("deploy/production/.env.example must declare AUTH_APPLICATION_JWT_AUDIENCE")
	}
	for system, expectation := range handoverSubsystemExpectations(audience) {
		t.Run(system, func(t *testing.T) {
			template := readEnvExample(t, filepath.Join(handoverDeployRoot, expectation.templatePath))
			manifest := readManifestValues(t, filepath.Join(handoverDeployRoot, expectation.manifestPath))
			for suffix, want := range expectation.keys {
				key := ""
				switch system {
				case "contract":
					key = "HANDOVER_MACHINE_" + suffix
				case "settlement":
					key = "SETTLEMENT_HANDOVER_" + suffix
				case "project":
					key = "HANDOVER_INTEGRATION_" + suffix
				}
				if key == "" {
					t.Fatalf("unknown subsystem %s", system)
				}
				templateValue, templateFound := template[key]
				if !templateFound {
					t.Fatalf("template %s is missing %s", expectation.templatePath, key)
				}
				if templateValue != want {
					t.Fatalf("template %s %s = %q, want %q", expectation.templatePath, key, templateValue, want)
				}
				manifestValue, manifestFound := manifest[key]
				if !manifestFound {
					t.Fatalf("manifest %s is missing %s", expectation.manifestPath, key)
				}
				if manifestValue != want {
					t.Fatalf("manifest %s %s = %q, want %q", expectation.manifestPath, key, manifestValue, want)
				}
			}
		})
	}
}
