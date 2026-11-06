package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// runtimeLicenseFixture 准备生产 Compose 目标的 V2 交付夹具：只依赖内置运行目录，
// 不再写入 runtime-license-*.json 批准文件。
func runtimeLicenseFixture(t *testing.T) (*productionComposeTarget, application.SubsystemProvisioningInput) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := productionSubsystemProfile{}
	profile.Manifest.Application.Code = "contract_management"
	profile.Manifest.Application.Environment = "prod"
	profile.Manifest.Compose.RuntimeServices = []string{"contract-api", "contract-worker"}
	target := &productionComposeTarget{}
	target.config.DeployRoot = root
	target.config.Profile = profile
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), "contract_management", "prod")
	if err != nil {
		t.Fatal(err)
	}
	input := application.SubsystemProvisioningInput{ApplicationCode: "contract_management", Environment: "prod", RuntimeLicenseSettings: application.RuntimeLicenseSettings{InstanceID: "installation-1", Environment: "production", PlatformBaseURL: "http://api:8080", PlatformPublicKeyPath: "/keys/license-platform-public.pem", StateDirectory: "/var/lib/license", AllowHTTP: true}}
	for i, component := range set.Components {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{
			Approval:             application.RuntimeLicenseApproval{ServiceID: component.ServiceID, Protocol: component.RegistrationProtocol},
			RegistrationProtocol: component.RegistrationProtocol,
			CatalogVersion:       component.CatalogVersion,
			CredentialGeneration: component.CredentialGeneration,
			ClientID:             component.ServiceID,
			ClientSecret:         "isolated-test-secret-" + string(rune('a'+i)),
		})
	}
	return target, input
}
