package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// localLicenseRunner 是本地 provisioner 的单测 runner：记录调用并给出可控的
// docker 输出。V1 审批文件已下线，夹具只准备 V2 目录所需的凭据。
type localLicenseRunner struct {
	calls [][]string
	image localImageMetadata
	fail  bool
}

func (r *localLicenseRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return nil
}

func (r *localLicenseRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if r.fail {
		return []byte("invalid"), nil
	}
	if len(args) > 0 && args[0] == "exec" {
		return []byte("1"), nil
	}
	if len(args) > 0 && args[0] == "ps" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "label=com.docker.compose.service=") {
				name := strings.TrimPrefix(arg, "label=com.docker.compose.service=")
				for i, id := range localLicenseServices["customer_and_opportunity"] {
					if id == name {
						return []byte(fmt.Sprintf("%064x", i+1)), nil
					}
				}
			}
		}
		return nil, nil
	}
	if len(args) > 1 && args[0] == "container" && args[1] == "inspect" {
		index, e := strconv.ParseInt(args[len(args)-1], 16, 64)
		if e != nil || index < 1 || index > 9 {
			return nil, fmt.Errorf("unexpected container")
		}
		return json.Marshal(map[string]any{"id": args[len(args)-1], "image": r.image.ID, "running": true, "started_at": time.Unix(1790000000, 0).UTC(), "project": "basic-platform-local", "service": localLicenseServices["customer_and_opportunity"][index-1], "oneoff": "False"})
	}
	return json.Marshal(r.image)
}

// localLicenseFixture 准备一个只依赖内置运行目录的本地 provisioner 夹具：
// 目录结构、compose 与镜像投影就绪，凭据按 customer_and_opportunity/dev 的
// 内置组件集合生成，不再写入任何 runtime-approvals 文件。
func localLicenseFixture(t *testing.T) (*LocalDockerSubsystemProvisioner, application.SubsystemProvisioningInput, *localLicenseRunner) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platform := filepath.Join(root, "platform")
	for _, path := range []string{"platform/scripts", "platform/runtime", "platform/data/keys", "platform/docker", "contract_management", "customer_and_opportunity", "customer_portal", "project_management", "Settlement", "data_analysis"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"platform/compose.local.yaml", "platform/docker/.env.local", "platform/docker/.env.customer.local", "platform/docker/.env.portal.local", "contract_management/.env.local", "project_management/.env.local", "Settlement/.env.local", "data_analysis/.env.local", "customer_and_opportunity/go.mod", "customer_and_opportunity/Dockerfile"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	image := localImageMetadata{ID: "sha256:" + strings.Repeat("a", 64), Version: "local", Protocol: "1"}
	runner := &localLicenseRunner{image: image}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root, GatewayScriptPath: filepath.Join(platform, "scripts", "portal-gateway.sh"), PlatformComposeProject: "basic-platform-local"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), "customer_and_opportunity", "dev")
	if err != nil {
		t.Fatal(err)
	}
	input := application.SubsystemProvisioningInput{ApplicationCode: "customer_and_opportunity", Environment: "dev", RuntimeLicenseSettings: application.RuntimeLicenseSettings{InstanceID: "dev-installation", Environment: "production", PlatformBaseURL: "http://platform-api:8080", PlatformPublicKeyPath: "/app/data/keys/jwt-ed25519-public.pem", StateDirectory: "/var/lib/commercial-license", AllowHTTP: true}}
	for _, component := range set.Components {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{
			Approval:             application.RuntimeLicenseApproval{ServiceID: component.ServiceID, Protocol: component.RegistrationProtocol},
			RegistrationProtocol: component.RegistrationProtocol,
			CatalogVersion:       component.CatalogVersion,
			CredentialGeneration: component.CredentialGeneration,
			ClientID:             "license-" + component.ServiceID,
			ClientSecret:         "secret-" + component.ServiceID,
		})
	}
	return p, input, runner
}
