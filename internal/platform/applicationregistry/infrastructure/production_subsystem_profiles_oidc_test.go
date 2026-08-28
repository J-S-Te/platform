package infrastructure

import "testing"

func TestValidateProductionOIDCValuesPortalCustomerBroker(t *testing.T) {
	err := validateProductionOIDCValues("customer_portal", map[string]string{"PORTAL_OIDC_IDP_HINT": "basic-platform-customer"})
	if err != nil {
		t.Fatalf("valid portal config rejected: %v", err)
	}
}

func TestValidateProductionOIDCValuesPortalInternalBrokerRejected(t *testing.T) {
	err := validateProductionOIDCValues("customer_portal", map[string]string{"PORTAL_OIDC_IDP_HINT": "basic-platform"})
	if err == nil {
		t.Fatal("portal using internal broker must be rejected")
	}
}

func TestValidateProductionPortalBrokerMissingHintRejected(t *testing.T) {
	files := []productionSubsystemRuntimeFileManifest{{Values: map[string]string{"OIDC_ISSUER": "x"}}}
	if err := validateProductionPortalBroker("customer_portal", files); err == nil {
		t.Fatal("portal without PORTAL_OIDC_IDP_HINT must be rejected")
	}
}

func TestValidateProductionPortalBrokerCustomerBrokerOK(t *testing.T) {
	files := []productionSubsystemRuntimeFileManifest{{Values: map[string]string{"PORTAL_OIDC_IDP_HINT": "basic-platform-customer"}}}
	if err := validateProductionPortalBroker("customer_portal", files); err != nil {
		t.Fatalf("valid portal rejected: %v", err)
	}
}

func TestValidateProductionPortalBrokerNonPortalOK(t *testing.T) {
	files := []productionSubsystemRuntimeFileManifest{{Values: map[string]string{"OIDC_IDP_HINT": "basic-platform"}}}
	if err := validateProductionPortalBroker("contract_management", files); err != nil {
		t.Fatalf("non-portal should skip check: %v", err)
	}
}

func TestValidateProductionOIDCValuesInternalBrokerOK(t *testing.T) {
	err := validateProductionOIDCValues("contract_management", map[string]string{"OIDC_IDP_HINT": "basic-platform"})
	if err != nil {
		t.Fatalf("valid internal config rejected: %v", err)
	}
}

func TestValidateProductionOIDCValuesInternalCustomerBrokerRejected(t *testing.T) {
	err := validateProductionOIDCValues("contract_management", map[string]string{"OIDC_IDP_HINT": "basic-platform-customer"})
	if err == nil {
		t.Fatal("internal subsystem using customer broker must be rejected")
	}
}

func TestValidateProductionOIDCValuesUnknownAliasRejected(t *testing.T) {
	err := validateProductionOIDCValues("contract_management", map[string]string{"OIDC_IDP_HINT": "some-other-idp"})
	if err == nil {
		t.Fatal("unknown alias must be rejected")
	}
}
