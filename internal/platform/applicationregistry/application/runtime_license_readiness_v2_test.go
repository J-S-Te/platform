package application

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/license-core/catalog"
)

func builtinConfirmationFixture() (coordination.Status, map[string]RuntimeLicenseCredential, time.Time) {
	now := time.Now().UTC()
	credentials := map[string]RuntimeLicenseCredential{
		"contract-api": {Approval: RuntimeLicenseApproval{ServiceID: "contract-api", Protocol: 2}, RegistrationProtocol: 2, CatalogVersion: catalog.Version, CredentialGeneration: "catalog-1", ClientID: "client-api"},
	}
	status := coordination.Status{
		Application:     coordination.Application{Application: "contract_management", Revision: 4, CatalogVersion: catalog.Version, RegistrationMode: catalog.BuiltinComponentV2},
		SnapshotCurrent: true,
		Members:         []coordination.Member{{ServiceID: "contract-api", Application: "contract_management", Environment: "prod", OAuthClientID: "client-api", RegistrationProtocol: 2, CatalogVersion: catalog.Version, ReadyAt: &now, AckAt: &now, IssuedRevision: 4, AckRevision: 4, ImageDigest: "sha256:old", CoverageDigest: "sha256:old"}},
	}
	return status, credentials, now
}

func TestV2ConfirmationUsesCatalogIdentityNotDigests(t *testing.T) {
	status, credentials, now := builtinConfirmationFixture()
	if !runtimeConfirmationCurrent(status, "contract_management", "prod", credentials, now) {
		t.Fatal("V2 confirmation rejected")
	}
	status.Members[0].ImageDigest = "sha256:new"
	status.Members[0].CoverageDigest = "sha256:new"
	if !runtimeConfirmationCurrent(status, "contract_management", "prod", credentials, now) {
		t.Fatal("V2 image rebuild invalidated confirmation")
	}
	status.Members[0].CatalogVersion = 0
	if runtimeConfirmationCurrent(status, "contract_management", "prod", credentials, now) {
		t.Fatal("V2 wrong catalog version accepted")
	}
}

func TestV1ConfirmationStillRequiresDigests(t *testing.T) {
	status, _, now := builtinConfirmationFixture()
	status.Application.RegistrationMode = catalog.LegacyImageV1
	status.Members[0].RegistrationProtocol = 1
	status.Members[0].CatalogVersion = 0
	credentials := map[string]RuntimeLicenseCredential{
		"contract-api": {Approval: RuntimeLicenseApproval{ServiceID: "contract-api", Protocol: 1, CoverageDigest: "sha256:coverage", ImageDigest: "sha256:image"}, ClientID: "client-api"},
	}
	status.Members[0].ImageDigest = "sha256:other"
	status.Members[0].CoverageDigest = "sha256:coverage"
	if runtimeConfirmationCurrent(status, "contract_management", "prod", credentials, now) {
		t.Fatal("V1 digest mismatch accepted")
	}
}
