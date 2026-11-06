package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// writeLocalLicensedOverrideFixture 写入与 deliverLocalBuiltinRuntimeLicense 同形的许可
// 固定记录（镜像钉定、私有 env 文件、license-state 卷、只读公钥挂载），供只校验特定
// 步骤的用例复用，避免它们真的走一遍镜像投影与目录登记。
func writeLocalLicensedOverrideFixture(t *testing.T, provisioner *LocalDockerSubsystemProvisioner, services ...string) string {
	t.Helper()
	root := provisioner.localPlatformRoot()
	override := localPinnedCompose{Services: map[string]localPinnedService{}, Volumes: map[string]struct{}{}}
	for _, service := range services {
		override.Services[service] = localPinnedService{
			PullPolicy: "never",
			Image:      "sha256:" + strings.Repeat("a", 64),
			EnvFile:    []string{filepath.Join(root, "runtime", "license-"+service+".env")},
			Volumes: []string{
				"license-state-" + service + ":/var/lib/commercial-license",
				filepath.Join(root, "data", "keys") + ":/app/data/keys:ro",
			},
		}
		override.Volumes["license-state-"+service] = struct{}{}
	}
	content, err := json.Marshal(override)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "runtime", "local-license.compose.json")
	if err = os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLocalLicensedRuntimeStateWithoutCredentialsFailsBeforeAnyComposeOperation 覆盖真实故障：
// 目标 .env.local 已声明 COMMERCIAL_LICENSE_ENABLED=true，而本次受控更新没有许可运行凭据。
// 此时容器必然启动即失败并每分钟重启，必须在任何 Compose 操作前以专用错误拦下。
func TestLocalLicensedRuntimeStateWithoutCredentialsFailsBeforeAnyComposeOperation(t *testing.T) {
	t.Parallel()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	platformRoot := filepath.Join(root, "platform")
	project := filepath.Join(root, "project_management")
	for _, directory := range []string{filepath.Join(platformRoot, "scripts"), filepath.Join(platformRoot, "docker"), filepath.Join(platformRoot, "runtime"), project} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gatewayScript := filepath.Join(platformRoot, "scripts", "portal-gateway.sh")
	for path, contents := range map[string]string{
		filepath.Join(platformRoot, "compose.local.yaml"):   "services: {}\n",
		filepath.Join(platformRoot, "docker", ".env.local"): "PLATFORM_SETTING=keep\n",
		gatewayScript: "#!/bin/sh\n",
		// 失败现场的真实形态：许可运行态键留在目标运行文件里，但本次操作没有凭据。
		filepath.Join(project, ".env.local"): "OIDC_TENANT_ID=tenant-a\nCOMMERCIAL_LICENSE_ENABLED=true\n" +
			"COMMERCIAL_LICENSE_STATE_PATH=/var/lib/commercial-license/project-api-0123456789abcdef.json\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root, GatewayScriptPath: gatewayScript,
		PlatformComposeProject: "basic-platform-local", Timeout: 10 * time.Second,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}

	// 受控更新与首次接入/接管都会创建容器，两条路径都必须 fail-closed。
	for _, entry := range []struct {
		name string
		run  func(context.Context, application.SubsystemProvisioningInput) error
	}{
		{"update", provisioner.Update},
		{"provision", provisioner.Provision},
	} {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			runner.calls = nil
			err := entry.run(context.Background(), application.SubsystemProvisioningInput{
				ApplicationCode: "project_management", Environment: "dev",
			})
			if err == nil {
				t.Fatal("licensed target without runtime credentials was accepted")
			}
			if !errors.Is(err, errLicenseRuntimeStateUnavailable) || !errors.Is(err, application.ErrRuntimeLicenseStateUnavailable) {
				t.Fatalf("error = %v, want license runtime state sentinel", err)
			}
			if !strings.Contains(err.Error(), "license runtime state is unavailable for project_management") {
				t.Fatalf("error text = %q, want application code in the diagnosis", err.Error())
			}
			if len(runner.calls) != 0 {
				t.Fatalf("compose/runner executed %d times before the license fail-closed check: %#v", len(runner.calls), runner.calls)
			}
		})
	}
}

// TestLocalLicensedRuntimeStateWithoutLicenseKeysKeepsRebuilding 确认 fail-closed 判定不会
// 误伤未登记商业运行时的目标：没有许可运行态键时照常返回 nil（既有更新语义不变）。
func TestLocalLicensedRuntimeStateWithoutLicenseKeysKeepsRebuilding(t *testing.T) {
	t.Parallel()
	p, input, _ := localLicenseFixture(t)
	input.RuntimeLicenseCredentials = nil
	if err := p.requireLocalLicenseRuntimeState(input); err != nil {
		t.Fatalf("unlicensed target was rejected: %v", err)
	}
}

// TestLocalLicensedRuntimeStateIgnoresUnlicensedCredentialFreeTarget 覆盖“凭据存在即放行”：
// 本次受控操作已交付许可凭据时，前置判定不得因为其它文件的内容而拦下部署。
func TestLocalLicensedRuntimeStateAllowsDeliveredCredentials(t *testing.T) {
	t.Parallel()
	p, input, _ := localLicenseFixture(t)
	path := filepath.Join(p.config.ProjectsRoot, input.ApplicationCode, ".env.local")
	if err := os.WriteFile(path, []byte("COMMERCIAL_LICENSE_ENABLED=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.requireLocalLicenseRuntimeState(input); err != nil {
		t.Fatalf("delivered credentials were rejected: %v", err)
	}
}

// TestLocalLicensedStackRequiresStateVolumeAndReadonlyKey 覆盖第二个静默缺口：凭据存在，
// 但固定记录没有声明 license-state 卷或只读公钥挂载时，绝不允许继续启动容器。
func TestLocalLicensedStackRequiresStateVolumeAndReadonlyKey(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"valid", "missing-override", "missing-volume", "missing-key-mount", "missing-env-file"} {
		scenario := scenario
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			p, input, runner := localLicenseFixture(t)
			input.ApplicationCode = "project_management"
			services := localLicenseServices["project_management"]
			switch scenario {
			case "missing-override":
				// 什么都不写：固定记录缺失。
			case "missing-volume", "missing-key-mount", "missing-env-file":
				path := writeLocalLicensedOverrideFixture(t, p, services...)
				override := localPinnedCompose{Services: map[string]localPinnedService{}, Volumes: map[string]struct{}{}}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &override); err != nil {
					t.Fatal(err)
				}
				target := override.Services[services[0]]
				switch scenario {
				case "missing-volume":
					delete(override.Volumes, "license-state-"+services[0])
				case "missing-key-mount":
					target.Volumes = []string{"license-state-" + services[0] + ":/var/lib/commercial-license"}
				case "missing-env-file":
					target.EnvFile = nil
				}
				override.Services[services[0]] = target
				if raw, err = json.Marshal(override); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				writeLocalLicensedOverrideFixture(t, p, services...)
			}

			err := p.requireLicensedLocalRuntimeVolumes(input)
			if scenario == "valid" {
				if err != nil {
					t.Fatalf("complete licensed override was rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, errLicenseRuntimeStateUnavailable) {
				t.Fatalf("error = %v, want license runtime state sentinel", err)
			}
			if !strings.Contains(err.Error(), "project_management") {
				t.Fatalf("error text = %q, want application code", err.Error())
			}
			if len(runner.calls) != 0 {
				t.Fatalf("container operations ran before the volume check: %#v", runner.calls)
			}
		})
	}
}

// TestProductionLicensedRuntimeStateWithoutCredentialsFailsClosed 覆盖生产侧同类缺口：
// 上一次受控接入留下的 runtime/license-<service>.env 仍要求商业许可运行态，而本次没有凭据。
func TestProductionLicensedRuntimeStateWithoutCredentialsFailsClosed(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := productionSubsystemProfile{}
	profile.Manifest.Application.Code = "project_management"
	profile.Manifest.Application.Environment = "prod"
	profile.Manifest.Compose.RuntimeServices = []string{"project-api"}
	runner := &recordingSubsystemRunner{}
	target := &productionComposeTarget{
		config: productionComposeTargetConfig{
			DeployRoot: root, RuntimeEnvPath: filepath.Join(root, "runtime", "project.env"), Profile: profile,
		},
		runner: runner,
	}
	if err = os.WriteFile(filepath.Join(root, "runtime", "license-project-api.env"),
		[]byte("COMMERCIAL_LICENSE_ENABLED=true\nCOMMERCIAL_LICENSE_STATE_PATH=/var/lib/commercial-license/project-api-ab.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = target.requireProductionLicenseRuntimeState(application.SubsystemProvisioningInput{
		ApplicationCode: "project_management", Environment: "prod",
	})
	if !errors.Is(err, errLicenseRuntimeStateUnavailable) {
		t.Fatalf("error = %v, want license runtime state sentinel", err)
	}
	if !strings.Contains(err.Error(), "project_management") {
		t.Fatalf("error text = %q, want application code", err.Error())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("production runner executed before the license fail-closed check: %#v", runner.calls)
	}

	// 未登记的组件（没有许可运行态文件）必须继续按兼容模式部署。
	target.config.Profile.Manifest.Compose.RuntimeServices = []string{"settlement-api"}
	if err = target.requireProductionLicenseRuntimeState(application.SubsystemProvisioningInput{ApplicationCode: "project_management", Environment: "prod"}); err != nil {
		t.Fatalf("unlicensed production target was rejected: %v", err)
	}
}

// TestComposeFailureDetailIsSingleLineTruncatedAndRedacted 覆盖错误透传的硬约束：
// last_error_message 是 varchar(1000)，摘要必须单行、限长、脱敏，并保持合法 UTF-8。
func TestComposeFailureDetailIsSingleLineTruncatedAndRedacted(t *testing.T) {
	t.Parallel()
	raw := "project-api | commercial license configuration failed\r\n" +
		"license client secret isolated-secret-value rejected\n" +
		strings.Repeat("许可运行态失败", 400)
	detail := singleLineFailureDetail(raw, []string{"isolated-secret-value"})
	if strings.ContainsAny(detail, "\r\n\t") {
		t.Fatalf("detail contains line breaks: %q", detail)
	}
	if strings.Contains(detail, "isolated-secret-value") || !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("detail did not redact the delivered secret: %q", detail)
	}
	if !strings.Contains(detail, "commercial license configuration failed") {
		t.Fatalf("detail lost the actionable cause: %q", detail)
	}
	if utf8.RuneCountInString(detail) > 800+len("...(truncated)") {
		t.Fatalf("detail is too long: %d runes", utf8.RuneCountInString(detail))
	}
	if !utf8.ValidString(detail) {
		t.Fatal("detail is not valid UTF-8")
	}
	// 直接编码成 JSON 也必须成功，且不产生转义后的换行。
	encoded, err := json.Marshal(map[string]string{"last_error_message": detail})
	if err != nil {
		t.Fatalf("detail is not JSON safe: %v", err)
	}
	if strings.Contains(string(encoded), `\n`) {
		t.Fatalf("encoded detail still carries escaped newlines: %s", encoded)
	}
}

// TestLocalRuntimeDeploymentErrorKeepsUnderlyingCause 覆盖错误链：外层短语必须与下层原因
// 一起返回，否则平台只能把“部署 Agent 执行失败”写进状态表。
func TestLocalRuntimeDeploymentErrorKeepsUnderlyingCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 1: compose output: project-api | commercial license configuration failed")
	err := localRuntimeDeploymentError(cause, "rebuild subsystem containers")
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("error = %v, want provisioning sentinel", err)
	}
	for _, want := range []string{"rebuild subsystem containers", "commercial license configuration failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	}
	if strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("error text must stay single line: %q", err.Error())
	}
}
