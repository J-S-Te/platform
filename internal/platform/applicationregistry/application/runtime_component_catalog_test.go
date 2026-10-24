package application

import (
	"testing"

	"github.com/J-S-Te/license-core/catalog"
)

func componentSet(serviceIDs ...string) RuntimeComponentSet {
	set := RuntimeComponentSet{Application: "contract_management", Environment: "prod", CatalogVersion: catalog.Version}
	for _, id := range serviceIDs {
		set.Components = append(set.Components, RuntimeComponentSpec{ServiceID: id, RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version, CredentialGeneration: "catalog-1"})
		set.RequiredServiceIDs = append(set.RequiredServiceIDs, id)
	}
	return set
}

func TestRuntimeComponentSetValidation(t *testing.T) {
	valid := componentSet("contract-api", "contract-worker")
	if err := ValidateRuntimeComponentSet(valid); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	missing := componentSet("contract-api")
	if err := ValidateRuntimeComponentSet(missing); err == nil {
		t.Fatal("missing required consumer accepted")
	}
	unknown := componentSet("contract-api", "contract-worker", "customer-api")
	if err := ValidateRuntimeComponentSet(unknown); err == nil {
		t.Fatal("cross-application component accepted")
	}
	infrastructure := componentSet("data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker", "data-analysis-metabase")
	infrastructure.Application = "data_analysis"
	if err := ValidateRuntimeComponentSet(infrastructure); err == nil {
		t.Fatal("Metabase accepted as a business consumer")
	}
	wrongProtocol := componentSet("contract-api", "contract-worker")
	wrongProtocol.Components[0].RegistrationProtocol = catalog.RegistrationProtocolV1
	if err := ValidateRuntimeComponentSet(wrongProtocol); err == nil {
		t.Fatal("V1 component accepted in V2 set")
	}
}

func TestSelectRuntimeComponentSetForPartialBridge(t *testing.T) {
	full := componentSet("contract-api", "contract-worker")
	partial, err := SelectRuntimeComponentSet(full, []string{"contract-worker"})
	if err != nil || len(partial.Components) != 1 || partial.Components[0].ServiceID != "contract-worker" || len(partial.RequiredServiceIDs) != 1 || partial.RequiredServiceIDs[0] != "contract-worker" {
		t.Fatalf("partial selection = %+v, %v", partial, err)
	}
	if _, err = SelectRuntimeComponentSet(full, []string{"contract-api", "contract-api"}); err == nil {
		t.Fatal("duplicate selection accepted")
	}
	if _, err = SelectRuntimeComponentSet(full, []string{"project-api"}); err == nil {
		t.Fatal("unknown selection accepted")
	}
	all, err := SelectRuntimeComponentSet(full, nil)
	if err != nil || len(all.Components) != 2 {
		t.Fatalf("empty selection did not retain full set: %+v, %v", all, err)
	}
}

func TestV2ClientIDStableAcrossImageChangesAndBounded(t *testing.T) {
	first := RuntimeLicenseClientIDV2("instance", "contract_management", "prod", "contract-api", "catalog-1")
	second := RuntimeLicenseClientIDV2("instance", "contract_management", "prod", "contract-api", "catalog-1")
	if first != second || len(first) > 128 {
		t.Fatalf("V2 client identity = %q / %q", first, second)
	}
	for _, changed := range []string{
		RuntimeLicenseClientIDV2("other-instance", "contract_management", "prod", "contract-api", "catalog-1"),
		RuntimeLicenseClientIDV2("instance", "project_management", "prod", "contract-api", "catalog-1"),
		RuntimeLicenseClientIDV2("instance", "contract_management", "dev", "contract-api", "catalog-1"),
		RuntimeLicenseClientIDV2("instance", "contract_management", "prod", "contract-worker", "catalog-1"),
		RuntimeLicenseClientIDV2("instance", "contract_management", "prod", "contract-api", "catalog-2"),
	} {
		if changed == first {
			t.Fatalf("identity did not change for a different binding: %q", changed)
		}
	}
}
