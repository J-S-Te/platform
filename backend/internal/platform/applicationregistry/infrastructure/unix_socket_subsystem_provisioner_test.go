package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	preflight := application.SubsystemPreflightInput{TenantID: "tenant-1", ApplicationCode: "contract_management", Environment: "dev"}
	if err := client.Preflight(context.Background(), preflight); err != nil {
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
	if !reflect.DeepEqual(received, input) {
		t.Fatalf("provision input = %#v, want %#v", received, input)
	}

	if err := client.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "contract_management", Environment: "dev",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := executor.updateInputSnapshot(); got.ApplicationCode != "contract_management" || got.Environment != "dev" {
		t.Fatalf("update input = %#v", got)
	}

	if err := client.Teardown(context.Background(), "tenant-1", "contract_management", "dev"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if gotCode, gotEnv := executor.teardownSnapshot(); gotCode != "contract_management" || gotEnv != "dev" {
		t.Fatalf("teardown (%q, %q)", gotCode, gotEnv)
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
	if err := client.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: "contract_management"}); !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("disabled preflight error = %v", err)
	}
}

func TestUnixSocketSubsystemProvisionerReportsProductionCapabilities(t *testing.T) {
	t.Parallel()
	client, err := NewUnixSocketSubsystemProvisioner(true, "/tmp/subsystem-provisioner.sock", time.Second, application.SubsystemProvisioningCapabilities{
		Mode: " production ", SupportedApplicationCodes: []string{" billing_management ", "billing_management"},
		SupportedEnvironments: []string{" dev "}, DefaultApplicationCode: " billing_management ",
		DefaultEnvironment: " dev ", DefaultPathPrefix: " /billing/ ",
	})
	if err != nil {
		t.Fatalf("construct production client: %v", err)
	}

	capabilities := client.Capabilities()
	if !capabilities.Enabled || capabilities.Mode != "production" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !reflect.DeepEqual(capabilities.SupportedApplicationCodes, []string{"billing_management"}) {
		t.Fatalf("supported application codes = %#v", capabilities.SupportedApplicationCodes)
	}
	if !reflect.DeepEqual(capabilities.SupportedEnvironments, []string{"dev"}) {
		t.Fatalf("supported environments = %#v", capabilities.SupportedEnvironments)
	}
	if capabilities.DefaultApplicationCode != "billing_management" || capabilities.DefaultEnvironment != "dev" || capabilities.DefaultPathPrefix != "/billing" {
		t.Fatalf("normalized defaults = %#v", capabilities)
	}
}

func TestUnixSocketSubsystemProvisionerRejectsInvalidMode(t *testing.T) {
	t.Parallel()

	invalidMode := application.SubsystemProvisioningCapabilities{Mode: "remote"}
	if client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second, invalidMode); err == nil || client != nil {
		t.Fatalf("invalid mode returned client=%#v err=%v", client, err)
	}
	if client, err := NewUnixSocketSubsystemProvisioner(false, "", time.Second, invalidMode, invalidMode); err == nil || client != nil {
		t.Fatalf("multiple policies returned client=%#v err=%v", client, err)
	}
}

func TestUnixSocketSubsystemProvisionerReturnsSafeActionableExecutorMessage(t *testing.T) {
	t.Parallel()
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-provisioner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "provisioner.sock")
	executor := &recordingSubsystemProvisioner{preflightErr: provisioningError("subsystem Compose file is unavailable")}
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	go func() { _ = RunSubsystemProvisioningServer(serverContext, socketPath, executor) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: "customer_and_opportunity"})
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) || !strings.Contains(err.Error(), "Compose file is unavailable") {
		t.Fatalf("preflight error = %v", err)
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
	mutex        sync.Mutex
	code         string
	input        application.SubsystemProvisioningInput
	teardownCode string
	teardownEnv  string
	updateInput  application.SubsystemProvisioningInput
	preflightErr error
}

func (provisioner *recordingSubsystemProvisioner) Preflight(_ context.Context, input application.SubsystemPreflightInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.code = input.ApplicationCode
	return provisioner.preflightErr
}

func (provisioner *recordingSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.input = input
	return nil
}

func (provisioner *recordingSubsystemProvisioner) Update(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.updateInput = input
	return nil
}

func (provisioner *recordingSubsystemProvisioner) Teardown(_ context.Context, _ string, code, environment string) error {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	provisioner.teardownCode = code
	provisioner.teardownEnv = environment
	return nil
}

func (provisioner *recordingSubsystemProvisioner) snapshot() (string, application.SubsystemProvisioningInput) {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.code, provisioner.input
}

func (provisioner *recordingSubsystemProvisioner) updateInputSnapshot() application.SubsystemProvisioningInput {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.updateInput
}

func (provisioner *recordingSubsystemProvisioner) teardownSnapshot() (string, string) {
	provisioner.mutex.Lock()
	defer provisioner.mutex.Unlock()
	return provisioner.teardownCode, provisioner.teardownEnv
}
