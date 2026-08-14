package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"testing"

	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
)

func TestReconcileStoredKeycloakClientVerifiesRealityBeforeMarkingReady(t *testing.T) {
	mapping := keycloakauthorizationinfrastructure.StoredKeycloakClientMapping{
		TenantID: "tenant", ApplicationID: "application", EnvironmentID: "environment",
		ApplicationName: "Orders", BaseURL: "http://orders.example.com", PathPrefix: "/orders",
		Realm: "basic-platform", ClientID: "orders-prod-web",
	}
	var calls []string
	dependencies := keycloakClientStartupReconcileDependencies{
		markPending: func(context.Context, string, string, string) error {
			calls = append(calls, "pending")
			return nil
		},
		ensureClient: func(_ context.Context, clientID, name, redirectURI string) (string, error) {
			calls = append(calls, "client:"+clientID+":"+name+":"+redirectURI)
			return clientID, nil
		},
		listRoleCodes: func(context.Context, string, string) ([]string, error) {
			calls = append(calls, "catalog")
			return []string{"admin", "viewer"}, nil
		},
		ensureRoles: func(_ context.Context, clientID string, roles []string) error {
			calls = append(calls, "roles:"+clientID+":"+roles[0]+":"+roles[1])
			return nil
		},
		saveMapping: func(context.Context, string, string, string, string, string) error {
			calls = append(calls, "save")
			return nil
		},
		markSynced: func(context.Context, string, string, string) error {
			calls = append(calls, "synced")
			return nil
		},
	}

	if err := reconcileStoredKeycloakClient(t.Context(), mapping, false, dependencies); err != nil {
		t.Fatalf("reconcileStoredKeycloakClient() error = %v", err)
	}
	want := []string{
		"pending",
		"client:orders-prod-web:Orders:http://orders.example.com/orders/auth/callback",
		"catalog",
		"roles:orders-prod-web:admin:viewer",
		"save",
		"synced",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestReconcileStoredKeycloakClientLeavesReadinessBlockedWhenAdminReconcileFails(t *testing.T) {
	mapping := keycloakauthorizationinfrastructure.StoredKeycloakClientMapping{
		TenantID: "tenant", ApplicationID: "application", EnvironmentID: "environment",
		ApplicationName: "Orders", BaseURL: "https://orders.example.com", PathPrefix: "/orders",
		Realm: "basic-platform", ClientID: "orders-prod-web",
	}
	adminErr := errors.New("admin unavailable")
	markedSynced := false
	dependencies := keycloakClientStartupReconcileDependencies{
		markPending: func(context.Context, string, string, string) error { return nil },
		ensureClient: func(context.Context, string, string, string) (string, error) {
			return "", adminErr
		},
		markSynced: func(context.Context, string, string, string) error {
			markedSynced = true
			return nil
		},
	}

	err := reconcileStoredKeycloakClient(t.Context(), mapping, true, dependencies)
	if !errors.Is(err, adminErr) {
		t.Fatalf("reconcileStoredKeycloakClient() error = %v, want %v", err, adminErr)
	}
	if markedSynced {
		t.Fatal("historical readiness was restored after the real Admin reconciliation failed")
	}
}
