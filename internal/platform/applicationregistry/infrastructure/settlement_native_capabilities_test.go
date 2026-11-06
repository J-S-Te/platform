package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestNativeSettlementAgentCapabilitiesFollowControlledMode 验证生产Agent只从审核目标读取明确模式。
// 参数：t为测试上下文，运行文件和发布镜像清单均为临时合成配置。
// 返回值：无；缺省不放宽、明确模式只输出目标环境且不泄漏配置。
func TestNativeSettlementAgentCapabilitiesFollowControlledMode(t *testing.T) {
	target, _ := settlementRoleRuntimeFixture(t, "settlement")
	path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
	release := filepath.Join(target.config.DeployRoot, ".release.env")
	if err := os.WriteFile(release, []byte("SETTLEMENT_IMAGE=registry.example/settlement@sha256:"+strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &ProductionComposeSubsystemProvisioner{enabled: true, releaseEnvPath: release,
		profiles: []productionSubsystemProfile{target.config.Profile},
		targets:  map[string]*productionComposeTarget{productionSubsystemTargetKey("settlement", "prod"): target}}
	for _, native := range []string{"false", "true"} {
		if err := updateProductionSubsystemEnvironment(path, map[string]string{
			"SETTLEMENT_NATIVE_FINANCIAL_ENABLED": native, "SETTLEMENT_INTEGRATION_ENABLED": "false"}); err != nil {
			t.Fatal(err)
		}
		caps, err := p.AvailableCapabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []string(nil)
		if native == "true" {
			want = []string{"prod"}
		}
		if !reflect.DeepEqual(caps.SettlementContractOptionalEnvironments, want) {
			t.Fatalf("mode=%s projection=%v", native, caps.SettlementContractOptionalEnvironments)
		}
		body, _ := json.Marshal(caps)
		if strings.Contains(string(body), target.config.DeployRoot) || strings.Contains(string(body), "isolated-") {
			t.Fatal("source projection leaked runtime configuration")
		}
	}
	if err := updateProductionSubsystemEnvironment(path, map[string]string{"SETTLEMENT_NATIVE_FINANCIAL_ENABLED": "typo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AvailableCapabilities(context.Background()); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

// TestNativeSettlementLocalAgentProjectsExactEnvironment 验证本地Agent使用实际环境且不回退别的模式。
// 参数：t为测试上下文，仅操作临时工作区文件。
// 返回值：无；非明确原生模式或错误布尔不能产生可选环境。
func TestNativeSettlementLocalAgentProjectsExactEnvironment(t *testing.T) {
	root, platformRoot, _, gateway := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	directory := filepath.Join(root, "Settlement")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ".env.local")
	if err := os.WriteFile(path, []byte("PLATFORM_ENVIRONMENT_CODE=test\nSETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\nSETTLEMENT_INTEGRATION_ENABLED=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{Enabled: true,
		ProjectsRoot: root, GatewayScriptPath: gateway, GatewayIncludePath: filepath.Join(platformRoot, "docker/gateway.conf")}}
	caps, err := p.AvailableCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caps.SettlementContractOptionalEnvironments, []string{"test"}) {
		t.Fatalf("wrong environment projection: %+v", caps)
	}
	if err := os.WriteFile(path, []byte("SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	caps, err = p.AvailableCapabilities(context.Background())
	if err != nil || len(caps.SettlementContractOptionalEnvironments) != 0 {
		t.Fatal("missing explicit disable became optional")
	}
}

// TestNativeSettlementUnixProjectionRejectsMalformedPolicy 验证真实Unix协议校验发生在规范化之前。
// 参数：t为测试上下文，Agent仅返回合成能力，不执行部署。
// 返回值：无；重复、跨应用目标及静态伪造策略必须拒绝。
func TestNativeSettlementUnixProjectionRejectsMalformedPolicy(t *testing.T) {
	valid := application.SubsystemProvisioningCapabilities{Enabled: true, Mode: "production",
		SupportedApplicationCodes: []string{"settlement"}, SupportedEnvironments: []string{"prod"},
		Targets:                                []application.SubsystemProvisioningTarget{{ApplicationCode: "settlement", Environment: "prod"}},
		SettlementContractOptionalEnvironments: []string{"prod"}}
	if _, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second, valid); err == nil {
		t.Fatal("runtime choice accepted as static authorization policy")
	}
	for _, mode := range []string{"valid", "duplicate", "other_target"} {
		t.Run(mode, func(t *testing.T) {
			capabilities := valid
			if mode == "duplicate" {
				capabilities.SettlementContractOptionalEnvironments = []string{"prod", "prod"}
			}
			if mode == "other_target" {
				capabilities.Targets = []application.SubsystemProvisioningTarget{{ApplicationCode: "project_management", Environment: "prod"}}
			}
			directory, err := os.MkdirTemp("/tmp", "settlement-cap-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			socket := filepath.Join(directory, "agent.sock")
			serverCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() {
				_ = RunSubsystemProvisioningServer(serverCtx, socket, &recordingSubsystemProvisioner{availableCapabilities: capabilities})
			}()
			waitForProvisioningSocket(t, socket)
			client, err := NewUnixSocketSubsystemProvisioner(true, socket, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := client.AvailableCapabilities(context.Background())
			if mode != "valid" {
				if err == nil {
					t.Fatal("malformed Agent policy accepted")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(actual.SettlementContractOptionalEnvironments, []string{"prod"}) {
				t.Fatalf("live projection failed: %+v %v", actual, err)
			}
		})
	}
}
