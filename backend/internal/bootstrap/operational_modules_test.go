package bootstrap

import (
	"reflect"
	"testing"
)

func TestInitialSubsystemAdministratorRoles(t *testing.T) {
	if got := initialSubsystemAdministratorRoles("contract_management"); !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("contract initial roles = %v", got)
	}
	if got := initialSubsystemAdministratorRoles("customer_and_opportunity"); !reflect.DeepEqual(got, []string{"sales_director", "team_lead", "technical_lead"}) {
		t.Fatalf("customer initial roles = %v", got)
	}
	if got := initialSubsystemAdministratorRoles("customer_portal"); len(got) != 0 {
		t.Fatalf("portal must not grant the internal onboarding operator a customer role: %v", got)
	}
}
