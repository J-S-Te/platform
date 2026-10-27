package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type observationSocketExecutor struct {
	recordingSubsystemProvisioner
	called bool
}

func (e *observationSocketExecutor) ObserveLicenseRuntime(context.Context, string, string) (licenseevidence.RuntimeObservation, error) {
	e.called = true
	return licenseevidence.RuntimeObservation{Status: "RUNNING", Available: true, CheckedAt: time.Now().UTC()}, nil
}

func TestLicenseRuntimeObservationSocketAction(t *testing.T) {
	for _, payload := range []bool{false, true} {
		left, right := net.Pipe()
		executor := &observationSocketExecutor{}
		done := make(chan struct{})
		go func() { handleSubsystemProvisioningConnection(context.Background(), left, executor); close(done) }()
		req := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_OBSERVATION", Code: "contract_management", Environment: "prod"}
		if payload {
			req.TenantID = "override"
		}
		if err := json.NewEncoder(right).Encode(req); err != nil {
			t.Fatal(err)
		}
		var reply subsystemProvisioningReply
		if err := json.NewDecoder(right).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		right.Close()
		<-done
		if payload {
			if reply.Success || executor.called {
				t.Fatal("unsupported observation payload executed")
			}
		} else if !reply.Success || !executor.called || reply.RuntimeObservation == nil || reply.RuntimeObservation.Status != "RUNNING" {
			t.Fatalf("observation reply lost: %+v", reply)
		}
	}
}

type observationRunner struct {
	ps     []byte
	config []byte
}

func (r *observationRunner) Run(ctx context.Context, directory string, environment []string, name string, args ...string) error {
	_, err := r.RunOutput(ctx, directory, environment, name, args...)
	return err
}

func (r *observationRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, " ps "):
		return r.ps, nil
	case strings.Contains(joined, " config "):
		return r.config, nil
	default:
		return nil, errors.New("unexpected observation command")
	}
}

func observationTarget(profile productionSubsystemProfile, runner *observationRunner) (*ProductionComposeSubsystemProvisioner, error) {
	target := &productionComposeTarget{
		config: productionComposeTargetConfig{
			DeployRoot: "/tmp/uip-observation", ComposeProject: "uip-observation",
			RuntimeEnvPath: "/tmp/uip-observation/.env", ReleaseEnvPath: "/tmp/uip-observation/.release.env",
			ComposeFile: "/tmp/uip-observation/docker-compose.yml", DockerBinary: "docker",
			Timeout: time.Second, Profile: profile,
		},
		runner: runner,
	}
	return &ProductionComposeSubsystemProvisioner{
		enabled: true,
		targets: map[string]*productionComposeTarget{
			productionSubsystemTargetKey("contract_management", "prod"): target,
		},
	}, nil
}

func TestProductionRuntimeObservationClassifiesTopology(t *testing.T) {
	profile := productionSubsystemProfile{Manifest: productionSubsystemManifest{
		Application: productionSubsystemApplicationManifest{Code: "contract_management", Environment: "prod"},
		Compose: productionSubsystemComposeManifest{
			RuntimeServices: []string{"contract-api", "contract-worker"},
		},
	}}
	config := []byte(`{"services":{"contract-api":{"image":"uip/contract:1"},"contract-worker":{"image":"uip/contract:1"}}}`)
	cases := []struct {
		name       string
		ps         string
		wantStatus string
		wantIssue  string
	}{
		{"running", `[{"Service":"contract-api","State":"running","Health":"healthy","Image":"uip/contract:1"},{"Service":"contract-worker","State":"running","Health":"healthy","Image":"uip/contract:1"}]`, "RUNNING", ""},
		{"partial", `[{"Service":"contract-api","State":"running","Health":"healthy","Image":"uip/contract:1"},{"Service":"contract-worker","State":"exited","Image":"uip/contract:1"}]`, "PARTIAL", "not_running"},
		{"not found", `[]`, "NOT_FOUND", "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provisioner, err := observationTarget(profile, &observationRunner{ps: []byte(tc.ps), config: config})
			if err != nil {
				t.Fatal(err)
			}
			observation, err := provisioner.ObserveLicenseRuntime(context.Background(), "contract_management", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if observation.Status != tc.wantStatus || !observation.Available || len(observation.Components) != 2 {
				t.Fatalf("observation = %+v", observation)
			}
			if tc.wantIssue != "" {
				found := false
				for _, component := range observation.Components {
					for _, issue := range component.Issues {
						if issue == tc.wantIssue {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("missing issue %s: %+v", tc.wantIssue, observation)
				}
			}
		})
	}
}

func TestProductionRuntimeObservationRejectsUnknownTarget(t *testing.T) {
	provisioner := &ProductionComposeSubsystemProvisioner{enabled: true}
	if _, err := provisioner.ObserveLicenseRuntime(context.Background(), "contract_management", "prod"); err == nil {
		t.Fatal("unknown target accepted")
	}
}
