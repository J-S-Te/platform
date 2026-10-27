package bootstrap

import (
	"context"
	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"strings"
	"testing"
)

type accessRuntimeImageStub struct {
	images   []registry.SubsystemImageReadiness
	reported bool
}

func (s accessRuntimeImageStub) ReadSubsystemAccessImages(context.Context, string, string) ([]registry.SubsystemImageReadiness, bool, error) {
	return s.images, s.reported, nil
}
func TestAccessRuntimeSelectionRequiresCompleteVerifiedConsumers(t *testing.T) {
	image := "example/contract@sha256:" + strings.Repeat("a", 64)
	for _, scenario := range []string{"complete", "missing worker", "unreported", "absent image"} {
		t.Run(scenario, func(t *testing.T) {
			images := []registry.SubsystemImageReadiness{{ServiceID: "contract-api", ImageDigest: image, Present: true}, {ServiceID: "contract-worker", ImageDigest: image, Present: true}, {ServiceID: "contract-migrate", ImageDigest: image, Present: true}}
			if scenario == "missing worker" {
				images = images[:1]
			}
			if scenario == "absent image" {
				images[1].Present = false
			}
			ids, err := subsystemAccessSelectedRuntimeServices(context.Background(), accessRuntimeImageStub{images: images, reported: scenario != "unreported"}, "contract_management", "prod")
			if scenario != "complete" {
				if err == nil {
					t.Fatal("incomplete topology accepted")
				}
				return
			}
			if err != nil || len(ids) != 2 || ids[0] != "contract-api" || ids[1] != "contract-worker" {
				t.Fatalf("selection %v %v", ids, err)
			}
		})
	}
}
