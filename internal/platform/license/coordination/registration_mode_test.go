package coordination

import (
	"errors"
	"strings"
	"testing"

	"github.com/J-S-Te/license-core/catalog"
)

func readyFixture(mode catalog.RegistrationMode, protocol, catalogVersion int) (Application, Member, ReadyInput) {
	app := Application{Application: "contract_management", RegistrationMode: mode, CatalogVersion: catalogVersion}
	image := "sha256:" + strings.Repeat("a", 64)
	coverage := "sha256:" + strings.Repeat("b", 64)
	member := Member{ServiceID: "contract-api", Application: "contract_management", Environment: "prod", RegistrationProtocol: protocol, CatalogVersion: catalogVersion, CoverageDigest: coverage, ImageDigest: image}
	input := ReadyInput{Protocol: 1, CoverageDigest: coverage, ImageDigest: image}
	return app, member, input
}

func TestV1StillRejectsDigestMismatch(t *testing.T) {
	app, member, input := readyFixture(catalog.LegacyImageV1, 1, 0)
	if err := validateReadyInput(app, member, "prod", input); err != nil {
		t.Fatalf("V1 correct digests rejected: %v", err)
	}
	input.ImageDigest = "sha256:" + strings.Repeat("c", 64)
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("V1 digest mismatch error = %v", err)
	}
}

func TestV2ReadyIgnoresImageRebuildButRequiresCatalogIdentity(t *testing.T) {
	app, member, _ := readyFixture(catalog.BuiltinComponentV2, 2, catalog.Version)
	input := ReadyInput{Protocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version}
	if err := validateReadyInput(app, member, "prod", input); err != nil {
		t.Fatalf("V2 ready without digests rejected: %v", err)
	}
	input.ImageDigest = "sha256:" + strings.Repeat("d", 64)
	input.CoverageDigest = "sha256:" + strings.Repeat("e", 64)
	if err := validateReadyInput(app, member, "prod", input); err != nil {
		t.Fatalf("V2 diagnostic digest change rejected: %v", err)
	}
	input.CatalogVersion = 0
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("V2 missing catalog version error = %v", err)
	}
}

func TestV2CannotSelfSelectMode(t *testing.T) {
	app, member, input := readyFixture(catalog.LegacyImageV1, 1, 0)
	input.Protocol = catalog.RegistrationProtocolV2
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("V1 application accepted V2 protocol: %v", err)
	}
	app, member, input = readyFixture(catalog.BuiltinComponentV2, 2, catalog.Version)
	input.Protocol = catalog.RegistrationProtocolV1
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("V2 application accepted V1 protocol: %v", err)
	}
	app, member, input = readyFixture(catalog.BuiltinComponentV2, 1, 0)
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("V2 application accepted V1 member: %v", err)
	}
}

func TestV2ReadyRejectsWrongMachineApplicationEnvironmentAndTenant(t *testing.T) {
	app, member, input := readyFixture(catalog.BuiltinComponentV2, 2, catalog.Version)
	wrongApplication := member
	wrongApplication.Application = "project_management"
	if err := validateReadyInput(app, wrongApplication, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrong application error = %v", err)
	}
	if err := validateReadyInput(app, member, "dev", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrong environment error = %v", err)
	}
	unknownMode := app
	unknownMode.RegistrationMode = "UNKNOWN_MODE"
	if err := validateReadyInput(unknownMode, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unknown mode error = %v", err)
	}
}

func TestV2SpecNeedsNoDigestsButUnknownProtocolFails(t *testing.T) {
	base := ServiceSpec{Application: "contract_management", Environment: "prod", ServiceID: "contract-api", OAuthClientID: "client-api"}
	legacy := base
	legacy.RegistrationProtocol = catalog.RegistrationProtocolV1
	if validSpec(legacy) {
		t.Fatal("V1 spec without digests accepted")
	}
	v2 := base
	v2.RegistrationProtocol = catalog.RegistrationProtocolV2
	v2.CatalogVersion = catalog.Version
	if !validSpec(v2) {
		t.Fatal("V2 spec without image/coverage digests rejected")
	}
	v2.RegistrationProtocol = 3
	if validSpec(v2) {
		t.Fatal("unknown registration protocol accepted")
	}
	v2.RegistrationProtocol = catalog.RegistrationProtocolV2
	v2.CatalogVersion = 0
	if validSpec(v2) {
		t.Fatal("V2 spec without catalog version accepted")
	}
}

func TestBridgeKeepsPerMemberProtocol(t *testing.T) {
	app, member, input := readyFixture(catalog.BridgingComponentV2, 1, 0)
	if err := validateReadyInput(app, member, "prod", input); err != nil {
		t.Fatalf("bridging V1 member rejected: %v", err)
	}
	app, member, _ = readyFixture(catalog.BridgingComponentV2, 2, catalog.Version)
	input = ReadyInput{Protocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version}
	if err := validateReadyInput(app, member, "prod", input); err != nil {
		t.Fatalf("bridging V2 member rejected: %v", err)
	}
	input.Protocol = catalog.RegistrationProtocolV1
	if err := validateReadyInput(app, member, "prod", input); !errors.Is(err, ErrNotReady) {
		t.Fatalf("bridging V2 member accepted V1 input: %v", err)
	}
}
