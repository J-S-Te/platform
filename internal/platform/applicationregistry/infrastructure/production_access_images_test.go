package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type accessProductionImageRunner struct{ missing, offline, mutable bool }

func (r *accessProductionImageRunner) Run(context.Context, string, []string, string, ...string) error {
	if r.offline {
		return errors.New("docker unavailable")
	}
	return nil
}
func (r *accessProductionImageRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if containsString(args, "--services") {
		return []byte("contract-api\ncontract-worker\ncontract-migrate\n"), nil
	}
	if len(args) > 1 && args[0] == "image" {
		if r.missing {
			return nil, errors.New("image absent")
		}
		return json.Marshal([]string{"example/contract@sha256:" + strings.Repeat("a", 64)})
	}
	image := "example/contract@sha256:" + strings.Repeat("a", 64)
	if r.mutable {
		image = "example/contract:latest"
	}
	return json.Marshal(map[string]any{"services": map[string]any{"contract-api": map[string]string{"image": image}, "contract-worker": map[string]string{"image": image}, "contract-migrate": map[string]string{"image": image}}})
}
func TestProductionAccessImageReadinessWithoutLegacyApproval(t *testing.T) {
	for _, scenario := range []string{"present", "missing", "offline", "mutable", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			target, _ := runtimeLicenseFixture(t)
			target.config.Profile.Manifest.Compose.MigrateService = "contract-migrate"
			target.config.DockerBinary = "docker"
			runner := &accessProductionImageRunner{missing: scenario == "missing", offline: scenario == "offline", mutable: scenario == "mutable"}
			target.runner = runner
			p := &ProductionComposeSubsystemProvisioner{enabled: true, targets: map[string]*productionComposeTarget{productionSubsystemTargetKey("contract_management", "prod"): target}}
			reader, ok := any(p).(subsystemImageReadinessProvider)
			if !ok {
				t.Fatal("production Agent does not report image readiness")
			}
			app := "contract_management"
			if scenario == "unknown" {
				app = "other"
			}
			images, err := reader.SubsystemImageReadiness(context.Background(), app, "prod")
			if scenario == "offline" || scenario == "mutable" || scenario == "unknown" {
				if err == nil {
					t.Fatal("invalid inventory accepted")
				}
				return
			}
			if err != nil || len(images) != 3 {
				t.Fatalf("inventory=%v err=%v", images, err)
			}
			for _, image := range images {
				if image.Present != (scenario == "present") {
					t.Fatalf("wrong presence: %v", image)
				}
			}
		})
	}
}
