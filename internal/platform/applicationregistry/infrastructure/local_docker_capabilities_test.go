package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalDockerAvailableCapabilitiesUsesExistingWorkspace(t *testing.T) {
	root := t.TempDir()
	files := []string{"platform/scripts/gateway.sh", "platform/compose.local.yaml", "platform/docker/.env.local", "platform/docker/.env.customer.local", "contract_management/.env.local", "project_management/.env.example", "Settlement/.env.example", "data_analysis/.env.example", "data_analysis/compose.yaml"}
	for _, path := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("secret-do-not-read"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "customer_and_opportunity"), 0700); err != nil {
		t.Fatal(err)
	}
	p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root, GatewayScriptPath: filepath.Join(root, "platform/scripts/gateway.sh"), GatewayIncludePath: filepath.Join(root, "platform/docker/gateway.conf")}}
	got, err := p.AvailableCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"contract_management", "customer_and_opportunity", "project_management", "settlement", "data_analysis"}
	if !got.Enabled || got.Mode != "local" || !reflect.DeepEqual(got.SupportedApplicationCodes, want) {
		t.Fatalf("unexpected local capabilities: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), "secret-do-not-read") || len(got.Targets) != 0 {
		t.Fatal("capabilities disclosed configuration or invented production targets")
	}
	if err := os.Remove(filepath.Join(root, "data_analysis/compose.yaml")); err != nil {
		t.Fatal(err)
	}
	got, err = p.AvailableCapabilities(context.Background())
	if err != nil || !reflect.DeepEqual(got.SupportedApplicationCodes, want[:4]) {
		t.Fatalf("removed Compose still advertised: %+v, %v", got, err)
	}
	socketDirectory, err := os.MkdirTemp("/tmp", "bp-local-cap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDirectory); err != nil {
			t.Errorf("clean temporary socket: %v", err)
		}
	})
	socketPath := filepath.Join(socketDirectory, "agent.sock")
	serverContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- RunSubsystemProvisioningServer(serverContext, socketPath, p) }()
	waitForProvisioningSocket(t, socketPath)
	client, err := NewUnixSocketSubsystemProvisioner(true, socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	live, err := client.AvailableCapabilities(context.Background())
	if err != nil || live.Mode != got.Mode || live.Enabled != got.Enabled || !reflect.DeepEqual(live.SupportedApplicationCodes, got.SupportedApplicationCodes) || !reflect.DeepEqual(live.SupportedEnvironments, got.SupportedEnvironments) || len(live.Targets) != 0 {
		t.Fatalf("real local executor capability dispatch failed: %+v, %v", live, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local capability server did not stop")
	}
}

func TestLocalDockerAvailableCapabilitiesDisabledMissingAndCanceled(t *testing.T) {
	for _, p := range []*LocalDockerSubsystemProvisioner{nil, {}, {config: LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: t.TempDir()}}} {
		got, err := p.AvailableCapabilities(context.Background())
		if err != nil || len(got.SupportedApplicationCodes) != 0 || got.Mode != "local" {
			t.Fatalf("missing configuration: %+v, %v", got, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.AvailableCapabilities(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled context ignored: %v", err)
		}
	}
}
