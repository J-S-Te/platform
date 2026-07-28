package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

func TestUnixSocketSubsystemProvisionerExchangesOnlySupportedOperations(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{}
	serverContext, cancelServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- RunSubsystemProvisioningServer(serverContext, socketPath, executor)
	}()
	waitForProvisioningSocket(t, socketPath)

	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("construct socket client: %v", err)
	}
	if err := client.Preflight(context.Background(), "contract_management"); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	input := application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "dev",
		Issuer: "http://localhost:8081", ClientID: "contract_management-dev-web",
		ClientSecret: "one-time-secret", RedirectURI: "http://localhost:8081/contract_management/auth/callback",
		PublicURL: "http://localhost:8081/contract_management/", PathPrefix: "/contract_management",
		UpstreamURL: "http://contract-api:8081",
	}
	if err := client.Provision(context.Background(), input); err != nil {
		t.Fatalf("provision: %v", err)
	}

	code, received := executor.snapshot()
	if code != "contract_management" {
		t.Fatalf("preflight code = %q", code)
	}
	if received != input {
		t.Fatalf("provision input = %#v, want %#v", received, input)
	}

	cancelServer()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provisioning server did not stop")
	}
}

func TestUnixSocketSubsystemProvisionerDisabled(t *testing.T) {
	t.Parallel()
	client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second)
	if err != nil {
		t.Fatalf("construct disabled client: %v", err)
	}
	if err := client.Preflight(context.Background(), "contract_management"); !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("disabled preflight error = %v", err)
	}
}

func waitForProvisioningSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provisioning socket %q was not created", socketPath)
}

type recordingSubsystemProvisioner struct {
	mutex sync.Mutex
	code  string
	input application.SubsystemProvisioningInput
}

func (provisioner *recordingSubsystemProvisioner) Preflight(_ context.Context, code string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.code = code
	return nil
}

func (provisioner *recordingSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.input = input
	return nil
}

func (provisioner *recordingSubsystemProvisioner) snapshot() (string, application.SubsystemProvisioningInput) {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.code, provisioner.input
}
