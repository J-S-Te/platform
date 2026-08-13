package infrastructure

import (
	"context"
	"testing"

	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
)

func TestNewSwitchReadinessStoreRejectsNilDatabase(t *testing.T) {
	if _, err := NewSwitchReadinessStore(nil); err == nil {
		t.Fatal("NewSwitchReadinessStore(nil) error = nil")
	}
}

func TestSwitchReadinessRequiresEveryGate(t *testing.T) {
	blocked := readinessFromRow(readinessRow{ClientReady: true, RoleCatalogSynced: true, UserProjectionCompleted: true})
	if blocked.SwitchReady || len(blocked.Gates) != 4 || blocked.Gates[3].Passed {
		t.Fatalf("incomplete readiness must fail closed: %#v", blocked)
	}
	ready := readinessFromRow(readinessRow{ClientReady: true, RoleCatalogSynced: true, UserProjectionCompleted: true, BrokerLoginVerified: true})
	if !ready.SwitchReady {
		t.Fatalf("complete readiness = %#v", ready)
	}
}

func TestSwitchReadinessEvidenceIsBoundToCurrentConfiguration(t *testing.T) {
	complete := readinessRow{
		ClientReady: true, RoleCatalogSynced: true, UserProjectionCompleted: true, BrokerLoginVerified: true,
		ClientConfigurationHash: "configuration-a", BrokerConfigurationHash: "configuration-a",
	}
	if got := readinessFromRow(bindReadinessToConfiguration(complete, "configuration-a")); !got.SwitchReady {
		t.Fatalf("matching configuration should be ready: %#v", got)
	}
	clientChanged := bindReadinessToConfiguration(complete, "configuration-b")
	if clientChanged.ClientReady || clientChanged.RoleCatalogSynced || clientChanged.UserProjectionCompleted || clientChanged.BrokerLoginVerified {
		t.Fatalf("Client configuration mismatch reused readiness: %#v", clientChanged)
	}
	brokerChanged := complete
	brokerChanged.BrokerConfigurationHash = "configuration-b"
	brokerChanged = bindReadinessToConfiguration(brokerChanged, "configuration-a")
	if !brokerChanged.ClientReady || !brokerChanged.RoleCatalogSynced || !brokerChanged.UserProjectionCompleted || brokerChanged.BrokerLoginVerified {
		t.Fatalf("stale broker evidence was not isolated: %#v", brokerChanged)
	}
}

func TestBrokerVerificationRequiresCompleteBoundInput(t *testing.T) {
	store, err := NewSwitchReadinessStore(newDryRunMySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBrokerLoginVerification(context.Background(), registryhttp.KeycloakBrokerLoginVerification{}); err == nil {
		t.Fatal("incomplete broker verification unexpectedly accepted")
	}
}
