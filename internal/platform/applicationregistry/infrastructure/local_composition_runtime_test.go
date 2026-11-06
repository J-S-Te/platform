package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type compositionRuntimeRunner struct{ recordingSubsystemRunner }

// RunOutput 提供项目镜像只读检查的受控测试结果。
// 参数：ctx 为测试上下文，dir/env 为执行边界，name/args 为实际命令。
// 返回值：合成不可变镜像身份；非预期命令返回错误。
func (r *compositionRuntimeRunner) RunOutput(ctx context.Context, dir string, env []string,
	name string, args ...string,
) ([]byte, error) {
	if err := r.Run(ctx, dir, env, name, args...); err != nil {
		return nil, err
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
		return []byte(`{"id":"sha256:` + strings.Repeat("a", 64) + `"}`), nil
	}
	return nil, errors.New("unexpected composition fixture output command")
}

// newCompositionRuntimeFixture 创建仅包含平台必需文件的隔离接入工作区。
// 参数：t 为当前测试；其临时目录承担全部文件与命令记录。
// 返回值：真实 provisioner、记录型 runner 以及合成工作区根目录。
func newCompositionRuntimeFixture(t *testing.T) (*LocalDockerSubsystemProvisioner,
	*compositionRuntimeRunner, string,
) {
	t.Helper()
	root := t.TempDir()
	platformRoot := filepath.Join(root, "platform")
	for _, relative := range []string{"platform/docker", "platform/scripts"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for relative, contents := range map[string]string{
		"platform/compose.local.yaml":        "services: {}\n",
		"platform/docker/.env.local":         "PLATFORM_SETTING=retained\n",
		"platform/docker/.env.lan.disabled":  "# empty LAN overrides\n",
		"platform/scripts/portal-gateway.sh": "#!/bin/sh\n",
	} {
		writeCompositionRuntime(t, root, relative, contents)
	}
	runner := &compositionRuntimeRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root,
		GatewayScriptPath:      filepath.Join(platformRoot, "scripts", "portal-gateway.sh"),
		GatewayIncludePath:     filepath.Join(platformRoot, "docker", "gateway.conf"),
		PlatformComposeProject: "composition-fixture",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, runner, root
}

// writeCompositionRuntime 向隔离工作区写入本用例所选模块的合成配置。
// 参数：t 为测试，root 为临时根目录，relative 为模块文件，contents 为非秘密测试内容。
// 返回值：无；目录或文件无法写入时终止测试。
func writeCompositionRuntime(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestLocalCompositionSelectedRuntime 验证单业务请求不依赖未安装业务的环境文件。
// 参数：t 为测试；各 case 只提供所选业务的实际运行文件。
// 返回值：无；请求受未选模块阻塞或 selected 被替换为空配置时失败。
func TestLocalCompositionSelectedRuntime(t *testing.T) {
	cases := []struct {
		name, path, key string
		arguments       []string
	}{
		{"contract", "contract_management/.env.local", "CONTRACT_RUNTIME_ENV_FILE",
			[]string{"up", "-d", "--no-deps", "contract-api"}},
		{"customer", "platform/docker/.env.customer.local", "CUSTOMER_RUNTIME_ENV_FILE",
			[]string{"run", "--rm", "--no-deps", "customer-api", "./authz-catalog", "publish", "crm"}},
		{"project", "project_management/.env.local", "PROJECT_RUNTIME_ENV_FILE",
			[]string{"run", "--rm", "--no-deps", "project-migrate"}},
		{"settlement", "Settlement/.env.local", "SETTLEMENT_RUNTIME_ENV_FILE",
			[]string{"run", "--rm", "--no-deps", "settlement-migrate"}},
		{"shared_temporal", "", "", []string{"up", "-d", "--no-deps", "contract-mysql", "temporal"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			p, runner, root := newCompositionRuntimeFixture(t)
			if test.path != "" {
				writeCompositionRuntime(t, root, test.path, "PLATFORM_ENVIRONMENT_CODE=prod\n")
			}
			if err := p.runIntegratedPlatformCompose(context.Background(), test.arguments...); err != nil {
				t.Fatalf("single-business request failed: %v", err)
			}
			call := runner.calls[len(runner.calls)-1]
			if !slices.Equal(call.arguments[len(call.arguments)-len(test.arguments):], test.arguments) {
				t.Fatalf("selected operation changed: %v", call.arguments)
			}
			if test.path != "" && !slices.Contains(call.environment, test.key+"="+filepath.Join(root, test.path)) {
				t.Fatal("selected runtime was not passed to Compose")
			}
			for key, path := range map[string]string{
				"CONTRACT_RUNTIME_ENV_FILE": "contract_management/.env.local",
				"CUSTOMER_RUNTIME_ENV_FILE": "platform/docker/.env.customer.local",
			} {
				if path != test.path && !slices.Contains(call.environment, key+"="+os.DevNull) {
					t.Fatalf("unselected missing runtime %s must resolve to empty parsing input", key)
				}
			}
		})
	}
}

// TestLocalCompositionMissingSelectedRuntime 验证实际启动与一次性任务不能使用空配置。
// 参数：t 为测试；各 case 提供全部非选中基础配置，但刻意缺少目标配置。
// 返回值：无；缺目标配置仍调用 runner 或返回成功时失败。
func TestLocalCompositionMissingSelectedRuntime(t *testing.T) {
	for _, service := range []string{"contract-api", "customer-migrate", "portal-api",
		"project-migrate", "settlement-worker", "dashboard-api", "portal-report-worker",
		"aggregation-worker", "alert-worker", "presale-worker"} {
		t.Run(service, func(t *testing.T) {
			p, runner, root := newCompositionRuntimeFixture(t)
			writeCompositionRuntime(t, root, "contract_management/.env.local", "CONTRACT_SETTING=keep\n")
			writeCompositionRuntime(t, root, "platform/docker/.env.customer.local", "CUSTOMER_SETTING=keep\n")
			if service == "contract-api" {
				if err := os.Remove(filepath.Join(root, "contract_management/.env.local")); err != nil {
					t.Fatal(err)
				}
			}
			if service == "customer-migrate" {
				if err := os.Remove(filepath.Join(root, "platform/docker/.env.customer.local")); err != nil {
					t.Fatal(err)
				}
			}
			err := p.runIntegratedPlatformCompose(context.Background(), "run", "--rm", "--no-deps", service)
			if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
				t.Fatalf("missing selected runtime must fail closed, got %v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("missing selected runtime reached runner: %d calls", len(runner.calls))
			}
		})
	}
}

// TestLocalCompositionPlatformFilesRemainRequired 验证平台自身配置仍是每次操作的前置条件。
// 参数：t 为测试；分别移除平台环境或 Compose 文件。
// 返回值：无；缺少平台文件仍发出命令时失败。
func TestLocalCompositionPlatformFilesRemainRequired(t *testing.T) {
	for _, relative := range []string{"platform/docker/.env.local", "platform/compose.local.yaml"} {
		t.Run(relative, func(t *testing.T) {
			p, runner, root := newCompositionRuntimeFixture(t)
			if err := os.Remove(filepath.Join(root, relative)); err != nil {
				t.Fatal(err)
			}
			if err := p.runIntegratedPlatformCompose(context.Background(), "build", "customer-api"); err == nil {
				t.Fatal("missing platform configuration accepted")
			}
			if len(runner.calls) != 0 {
				t.Fatal("missing platform configuration reached runner")
			}
		})
	}
}

// TestLocalCompositionPreflightUsesSelectedTemplate 验证首次接入不要求尚未生成的运行文件。
// 参数：t 为测试；只提供平台文件与选中项目的配置模板。
// 返回值：无；未选合同/CRM缺文件阻塞预检或预检擅自启动容器时失败。
func TestLocalCompositionPreflightUsesSelectedTemplate(t *testing.T) {
	p, runner, root := newCompositionRuntimeFixture(t)
	writeCompositionRuntime(t, root, "project_management/.env.example", "PROJECT_SETTING=template\n")
	if err := p.Preflight(context.Background(), application.SubsystemPreflightInput{
		ApplicationCode: integratedProjectApplicationCode,
	}); err != nil {
		t.Fatalf("selected template preflight failed: %v", err)
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0].arguments,
		[]string{"version", "--format", "{{.Server.Version}}"}) {
		t.Fatal("preflight must only check Docker availability")
	}
	if _, err := os.Stat(filepath.Join(root, "project_management", ".env.local")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight must not generate selected runtime")
	}
}
