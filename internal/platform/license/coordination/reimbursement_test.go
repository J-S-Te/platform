package coordination

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

// reimbursement 是目录内第七个商业应用。协调器必须把它当作一等应用：
// 否则登记会在 validSpec 被拒、状态接口缺行、快照绑定失败。
func TestReimbursementIsCoordinatedCatalogApplication(t *testing.T) {
	applications := catalog.Applications()
	found := false
	for _, application := range applications {
		if application == "reimbursement" {
			found = true
		}
		if !validApplication(application) {
			t.Errorf("catalog application %q rejected by coordination whitelist", application)
		}
	}
	if !found {
		t.Fatal("catalog no longer contains reimbursement")
	}
	if validApplication("platform") || validApplication("unknown_app") {
		t.Fatal("coordination whitelist accepted a non-business application")
	}
	components, err := catalog.Components("reimbursement")
	if err != nil {
		t.Fatal(err)
	}
	business, err := builtinBusinessComponents("reimbursement", components)
	if err != nil {
		t.Fatalf("reimbursement catalog cannot bridge: %v", err)
	}
	if len(business) != 2 {
		t.Fatalf("reimbursement business components = %d, want 2", len(business))
	}
	for _, component := range business {
		// 登记入口以目录身份构造 V2 规格；报销的两个服务都必须通过协调层校验。
		spec := ServiceSpec{
			Application:          "reimbursement",
			Environment:          "production",
			ServiceID:            component.ServiceID,
			OAuthClientID:        catalog.ClientIDV2("instance", "reimbursement", "production", component.ServiceID, "catalog-1"),
			RegistrationProtocol: catalog.RegistrationProtocolV2,
			CatalogVersion:       catalog.Version,
		}
		if !validSpec(spec) {
			t.Errorf("reimbursement spec %q rejected: %+v", component.ServiceID, spec)
		}
	}
}

// 就绪门禁必须按目录身份接受报销成员，而不是回退到六个已知应用的隐式假设。
func TestReimbursementReadyUsesCatalogIdentity(t *testing.T) {
	app := Application{Application: "reimbursement", RegistrationMode: catalog.BuiltinComponentV2, CatalogVersion: catalog.Version}
	member := Member{ServiceID: "reimbursement-api", Application: "reimbursement", Environment: "production", RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version}
	input := ReadyInput{Protocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version}
	if err := validateReadyInput(app, member, "production", input); err != nil {
		t.Fatalf("reimbursement V2 ready rejected: %v", err)
	}
	wrongEnvironment := member
	wrongEnvironment.Application = "contract_management"
	if err := validateReadyInput(app, wrongEnvironment, "production", input); err == nil {
		t.Fatal("reimbursement ready accepted a foreign member")
	}
	wrongCatalog := input
	wrongCatalog.CatalogVersion = 0
	if err := validateReadyInput(app, member, "production", wrongCatalog); err == nil {
		t.Fatal("reimbursement ready accepted a missing catalog version")
	}
}

// 快照签名/验签走运行时绑定白名单：报销的目录服务必须能签发可复验快照，
// 白名单外的应用仍必须被拒绝。
func TestReimbursementSnapshotIsBoundToCatalogService(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vendorPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"test-vendor": vendorPub}
	now := time.Unix(1900000000, 0).UTC()
	service := &Service{signer: testSigner{priv: priv, pub: pub, vendor: keys}, vendor: keys, now: func() time.Time { return now }}
	deployment := &domain.Deployment{InstanceID: "instance", Environment: "production", HighestVersion: 3}
	application := Application{Application: "reimbursement", State: runtime.Enforced, Revision: 4}
	raw, err := service.signer.Sign(runtime.PlatformSnapshot{
		Protocol: runtime.Protocol, InstanceID: deployment.InstanceID, Environment: deployment.Environment,
		Application: application.Application, ServiceID: "reimbursement-api", Revision: application.Revision,
		EnforcementState: application.State, HighestLicenseVersion: deployment.HighestVersion, SnapshotIssuedAt: now.Unix(),
	})
	if err != nil {
		t.Fatalf("sign reimbursement snapshot: %v", err)
	}
	member := Member{ServiceID: "reimbursement-api", Application: application.Application, RawJWS: raw}
	if err := service.verifyIssued(member, application, deployment); err != nil {
		t.Fatalf("verify reimbursement snapshot: %v", err)
	}
	if _, err := service.signer.Sign(runtime.PlatformSnapshot{
		Protocol: runtime.Protocol, InstanceID: deployment.InstanceID, Environment: deployment.Environment,
		Application: "legacy_app", ServiceID: "legacy-api", Revision: 1, EnforcementState: runtime.Pending,
		HighestLicenseVersion: deployment.HighestVersion, SnapshotIssuedAt: now.Unix(),
	}); err == nil {
		t.Fatal("snapshot signed for an application outside the binding whitelist")
	}
}
