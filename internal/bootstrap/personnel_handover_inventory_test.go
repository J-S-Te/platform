package bootstrap

import (
	"context"
	"errors"
	"testing"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	coordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	licenseruntime "github.com/J-S-Te/license-core/runtime"
)

type inventoryRegistryFake struct {
	applications []applicationregistryapplication.Application
	environments map[string][]applicationregistryapplication.Environment
	err          error
}

func (fake inventoryRegistryFake) ListApplications(context.Context, string, applicationregistryapplication.PageRequest) (applicationregistryapplication.PageResult[applicationregistryapplication.Application], error) {
	if fake.err != nil {
		return applicationregistryapplication.PageResult[applicationregistryapplication.Application]{}, fake.err
	}
	return applicationregistryapplication.PageResult[applicationregistryapplication.Application]{Items: fake.applications}, nil
}

func (fake inventoryRegistryFake) ListEnvironments(_ context.Context, _ string, applicationID string, _ applicationregistryapplication.PageRequest) (applicationregistryapplication.PageResult[applicationregistryapplication.Environment], error) {
	return applicationregistryapplication.PageResult[applicationregistryapplication.Environment]{Items: fake.environments[applicationID]}, nil
}

type inventoryLicenseFake struct {
	statuses map[string]coordination.Status
	err      error
}

func (fake inventoryLicenseFake) Status(_ context.Context, application string) (coordination.Status, error) {
	if fake.err != nil {
		return coordination.Status{}, fake.err
	}
	return fake.statuses[application], nil
}

func inventoryFor(registry handoverRegistryReader, licenses handoverLicenseStatusReader) *personnelHandoverInventory {
	return &personnelHandoverInventory{registry: registry, licenses: licenses, systems: []string{"contract_management", "customer_and_opportunity"}, environment: "prod"}
}

func registryWithContract(environments ...applicationregistryapplication.Environment) inventoryRegistryFake {
	return inventoryRegistryFake{
		applications: []applicationregistryapplication.Application{{ID: "app-contract", Code: "contract_management"}},
		environments: map[string][]applicationregistryapplication.Environment{"app-contract": environments},
	}
}

func capabilityOf(t *testing.T, states []identityapplication.HandoverSystemState, system string) string {
	t.Helper()
	for _, state := range states {
		if state.System == system {
			return state.Capability
		}
	}
	t.Fatalf("system %s missing from inventory", system)
	return ""
}

func TestHandoverInventoryClassifiesRegisteredSystems(t *testing.T) {
	registry := registryWithContract(applicationregistryapplication.Environment{ID: "env-1", Environment: "prod", Status: "ACTIVE"})
	licenses := inventoryLicenseFake{statuses: map[string]coordination.Status{
		"contract_management": {Application: coordination.Application{State: licenseruntime.Enforced}, SnapshotCurrent: true},
	}}
	states, err := inventoryFor(registry, licenses).Systems(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := capabilityOf(t, states, "contract_management"); got != identityapplication.HandoverSystemApplicable {
		t.Fatalf("contract capability = %s, want APPLICABLE", got)
	}
	if got := capabilityOf(t, states, "customer_and_opportunity"); got != identityapplication.HandoverSystemNotInstalled {
		t.Fatalf("unregistered system = %s, want NOT_INSTALLED (不适用)", got)
	}
}

func TestHandoverInventoryRequiresTargetEnvironment(t *testing.T) {
	registry := registryWithContract(applicationregistryapplication.Environment{ID: "env-1", Environment: "dev", Status: "ACTIVE"})
	states, err := inventoryFor(registry, nil).Systems(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := capabilityOf(t, states, "contract_management"); got != identityapplication.HandoverSystemNotInstalled {
		t.Fatalf("capability = %s, want NOT_INSTALLED when the target environment is absent", got)
	}
}

func TestHandoverInventoryBlocksOnLicenseState(t *testing.T) {
	registry := registryWithContract(applicationregistryapplication.Environment{ID: "env-1", Environment: "prod", Status: "ACTIVE"})
	cases := map[string]coordination.Status{
		"pending":       {Application: coordination.Application{State: licenseruntime.Pending}, SnapshotCurrent: true},
		"stale-snapshot": {Application: coordination.Application{State: licenseruntime.Enforced}, SnapshotCurrent: false},
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			states, err := inventoryFor(registry, inventoryLicenseFake{statuses: map[string]coordination.Status{"contract_management": status}}).Systems(context.Background(), "tenant-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := capabilityOf(t, states, "contract_management"); got != identityapplication.HandoverSystemLicenseDenied {
				t.Fatalf("capability = %s, want LICENSE_DENIED", got)
			}
		})
	}
}

func TestHandoverInventoryTreatsLicenseReadFailureAsUnreachable(t *testing.T) {
	registry := registryWithContract(applicationregistryapplication.Environment{ID: "env-1", Environment: "prod", Status: "ACTIVE"})
	states, err := inventoryFor(registry, inventoryLicenseFake{err: errors.New("license store unavailable")}).Systems(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := capabilityOf(t, states, "contract_management"); got != identityapplication.HandoverSystemUnreachable {
		t.Fatalf("capability = %s, want UNREACHABLE", got)
	}
}

func TestHandoverInventoryWithoutLicenseReaderStaysApplicable(t *testing.T) {
	registry := registryWithContract(applicationregistryapplication.Environment{ID: "env-1", Environment: "prod", Status: "ACTIVE"})
	states, err := inventoryFor(registry, nil).Systems(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := capabilityOf(t, states, "contract_management"); got != identityapplication.HandoverSystemApplicable {
		t.Fatalf("capability = %s, want APPLICABLE", got)
	}
}
