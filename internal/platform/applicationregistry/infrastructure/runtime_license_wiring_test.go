package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestProductionRejectsUnloadedLicenseCredentials 用途：确保凭据落盘不能代替组件实际加载许可配置。
// 参数：t 为不连接业务环境的测试上下文。
// 返回值：无。
func TestProductionRejectsUnloadedLicenseCredentials(t *testing.T) {
	p, _, _ := productionProvisionerFixture(t)
	target, err := p.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	input := productionContractInput("https://platform.example.com")
	_, binding := runtimeLicenseFixture(t)
	input.RuntimeLicenseSettings = binding.RuntimeLicenseSettings
	for _, id := range target.config.Profile.Manifest.Compose.RuntimeServices {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{
			Approval:             application.RuntimeLicenseApproval{ServiceID: id, Protocol: 2},
			RegistrationProtocol: 2, CatalogVersion: 1, CredentialGeneration: "catalog-1",
			ClientID: "builtin-" + id, ClientSecret: "isolated-secret", RetirementNotRequired: true,
		})
	}
	err = p.Provision(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "LICENSE_RUNTIME_DELIVERY_FAILED") {
		t.Fatalf("accepted missing resolved Compose license configuration: %v", err)
	}
}

type licensedComposeFixtureRunner struct {
	subsystemCommandRunner
	input application.SubsystemProvisioningInput
}

// RunOutput 用途：为完整部署测试模拟 Compose 对已生成私有 env 文件的实际解析。
// 参数：ctx 为上下文，directory 为测试部署目录，environment 和 name 为命令参数，args 为子命令。
// 返回值：返回隔离测试编排或透传既有测试 runner 的输出及错误。
func (r *licensedComposeFixtureRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, args ...string) ([]byte, error) {
	if containsString(args, "--format") && containsString(args, "json") && containsString(args, "config") {
		services := map[string]runtimeLicenseComposeService{}
		for _, credential := range r.input.RuntimeLicenseCredentials {
			id := credential.Approval.ServiceID
			raw, err := os.ReadFile(filepath.Join(directory, "runtime", "license-"+id+".env"))
			if err != nil {
				return nil, err
			}
			services[id] = runtimeLicenseComposeService{Environment: parseEnvironmentValues(string(raw)), Volumes: []runtimeLicenseComposeMount{
				{Type: "volume", Source: "license-state-" + id, Target: r.input.RuntimeLicenseSettings.StateDirectory},
				{Type: "bind", Source: "test-public.pem", Target: r.input.RuntimeLicenseSettings.PlatformPublicKeyPath, ReadOnly: true},
			}}
		}
		return json.Marshal(map[string]any{"services": services})
	}
	return r.subsystemCommandRunner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	}).RunOutput(ctx, directory, environment, name, args...)
}

// TestLicenseConsumerWiringRejectsDrift 用途：覆盖许可覆盖、凭据错配、状态共享及公钥可写等编排漂移。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestLicenseConsumerWiringRejectsDrift(t *testing.T) {
	for _, scenario := range []string{"valid", "disabled", "wrong-secret", "missing-state", "shared-state", "readonly-state", "writable-key"} {
		t.Run(scenario, func(t *testing.T) {
			expected := map[string]string{"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_CLIENT_SECRET": "isolated-secret"}
			actual := runtimeLicenseComposeService{Environment: map[string]string{"COMMERCIAL_LICENSE_ENABLED": "true", "COMMERCIAL_LICENSE_CLIENT_SECRET": "isolated-secret"}, Volumes: []runtimeLicenseComposeMount{
				{Type: "volume", Source: "private-state", Target: "/state"},
				{Type: "bind", Source: "public.pem", Target: "/public.pem", ReadOnly: true},
			}}
			states := map[string]bool{}
			switch scenario {
			case "disabled":
				actual.Environment["COMMERCIAL_LICENSE_ENABLED"] = "false"
			case "wrong-secret":
				actual.Environment["COMMERCIAL_LICENSE_CLIENT_SECRET"] = "other-secret"
			case "missing-state":
				actual.Volumes[0].Type = "bind"
			case "shared-state":
				states["private-state"] = true
			case "readonly-state":
				actual.Volumes[0].ReadOnly = true
			case "writable-key":
				actual.Volumes[1].ReadOnly = false
			}
			err := validateRuntimeLicenseConsumer("service", actual, expected,
				application.RuntimeLicenseSettings{StateDirectory: "/state", PlatformPublicKeyPath: "/public.pem"}, states)
			if (err != nil) != (scenario != "valid") {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "isolated-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}
