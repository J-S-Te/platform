package infrastructure

import (
	"context"
	"testing"
)

func TestBuiltinRuntimeComponentCatalogMatchesProfiles(t *testing.T) {
	source := BuiltinRuntimeComponentCatalog{}
	contract, err := source.RuntimeComponentSet(context.Background(), "contract_management", "prod")
	if err != nil || len(contract.Components) != 2 || len(contract.RequiredServiceIDs) != 2 {
		t.Fatalf("contract set = %+v, err = %v", contract, err)
	}
	data, err := source.RuntimeComponentSet(context.Background(), "data_analysis", "prod")
	if err != nil || len(data.Components) != 3 || len(data.RequiredServiceIDs) != 3 {
		t.Fatalf("data set = %+v, err = %v", data, err)
	}
	for _, component := range data.Components {
		if component.ServiceID == "data-analysis-metabase" {
			t.Fatal("Metabase was enrolled as a business consumer")
		}
	}
	portal, err := source.RuntimeComponentSet(context.Background(), "customer_portal", "prod")
	if err != nil || len(portal.Components) != 3 || len(portal.RequiredServiceIDs) != 2 {
		t.Fatalf("portal set = %+v, err = %v", portal, err)
	}
	if _, err := source.RuntimeComponentSet(context.Background(), "unknown_application", "prod"); err == nil {
		t.Fatal("unknown application accepted")
	}
	if _, err := source.RuntimeComponentSet(context.Background(), "contract_management", ""); err == nil {
		t.Fatal("missing environment accepted")
	}
}
