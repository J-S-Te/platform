package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"os"
	"path/filepath"
	"strings"
)

func localProjectBackgroundEnvironment(service, image string) map[string]string {
	if (service != "project-api" && service != "sla-notifier") || !localRuntimeDigest.MatchString(image) {
		return nil
	}
	return map[string]string{"PROJECT_BACKGROUND_BUILD_ID": "project-background-sha256-" + strings.TrimPrefix(image, "sha256:")}
}

func projectNotificationRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	credential, ok := input.ServiceCredential(application.ServiceCredentialNotificationIngest)
	if !ok || strings.TrimSpace(credential.OAuthClient.ClientID) == "" || strings.TrimSpace(credential.PlaintextSecret) == "" {
		return provisioningError("project background notification identity is missing; use controlled retry to provision notification_ingest")
	}
	values["NOTIFICATION_ENABLED"] = "true"
	values["PLATFORM_NOTIFICATION_URL"] = "http://platform-api:8080/api/v1/notifications/events"
	values["PLATFORM_NOTIFICATION_SCOPE"] = "notification.ingest"
	values["PLATFORM_NOTIFICATION_CLIENT_ID"] = credential.OAuthClient.ClientID
	values["PLATFORM_NOTIFICATION_CLIENT_SECRET"] = credential.PlaintextSecret
	return nil
}

func validateLocalProjectBackgroundEnvironment(service, image string, environment map[string]string) error {
	// Existing records remain readable; the next controlled delivery adds this
	// derived field while keeping the already approved image identity unchanged.
	if len(environment) == 0 {
		return nil
	}
	expected := localProjectBackgroundEnvironment(service, image)
	if len(expected) != 1 || len(environment) != 1 || environment["PROJECT_BACKGROUND_BUILD_ID"] != expected["PROJECT_BACKGROUND_BUILD_ID"] {
		return errors.New("local project background configuration must derive only its build ID from the approved image pin")
	}
	return nil
}

// Local consumers use Docker's immutable config ID rather than a registry
// manifest digest. The shared lifecycle still compares the actual running image.
type localProjectBackground struct {
	provisioner         *LocalDockerSubsystemProvisioner
	tenant, environment string
	operationContext    context.Context
}

func (p *LocalDockerSubsystemProvisioner) projectBackgroundTarget(ctx context.Context) (*localProjectBackground, error) {
	values, err := readEnvironmentValues(filepath.Join(p.config.ProjectsRoot, "project_management", ".env.local"))
	if err != nil {
		return nil, err
	}
	tenant := strings.TrimSpace(values["OIDC_TENANT_ID"])
	environment := localRuntimeEnvironment(filepath.Join(p.config.ProjectsRoot, "project_management", ".env.local"), "project_management", "OIDC_CLIENT_ID")
	if tenant == "" || environment == "" {
		return nil, errors.New("trusted project runtime tenant/environment is missing")
	}
	return &localProjectBackground{provisioner: p, tenant: tenant, environment: environment, operationContext: ctx}, nil
}

func (p *LocalDockerSubsystemProvisioner) localProjectBackgroundImage(ctx context.Context) (string, error) {
	path, err := p.localRuntimeOverride()
	if err != nil {
		return "", err
	}
	if path != "" {
		var pinned localPinnedCompose
		if err := readLocalPrivateJSON(path, &pinned); err != nil {
			return "", err
		}
		api, apiOK := pinned.Services["project-api"]
		background, bgOK := pinned.Services["sla-notifier"]
		if apiOK || bgOK {
			if !apiOK || !bgOK || api.Image != background.Image {
				return "", errors.New("project consumers require the same complete approved image pin")
			}
			return api.Image, nil
		}
	}
	image, err := p.localImageProjection(ctx, "project-management/backend:local")
	if err != nil {
		return "", err
	}
	if !localRuntimeDigest.MatchString(image.ID) {
		return "", errors.New("local project image has no immutable config identity")
	}
	return image.ID, nil
}
func (b *localProjectBackground) projectBackgroundRelease() (string, string, error) {
	image, err := b.provisioner.localProjectBackgroundImage(b.ctx())
	if err != nil {
		return "", "", err
	}
	build, err := b.projectBackgroundImageBuildID(image)
	return image, build, err
}

// Release resolution is bounded even if the caller is recovering a saved operation.
func (b *localProjectBackground) ctx() context.Context { return b.operationContext }
func (b *localProjectBackground) projectBackgroundImageBuildID(image string) (string, error) {
	if !localRuntimeDigest.MatchString(image) {
		return "", errors.New("local project background requires immutable config ID")
	}
	return "project-background-sha256-" + strings.TrimPrefix(image, "sha256:"), nil
}
func (b *localProjectBackground) stateStore() *productionComposeTarget {
	return &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: b.provisioner.localPlatformRoot()}}
}
func (b *localProjectBackground) loadProjectBackgroundOperation() (*projectBackgroundOperation, error) {
	return b.stateStore().loadProjectBackgroundOperation()
}
func (b *localProjectBackground) saveProjectBackgroundOperation(operation *projectBackgroundOperation) error {
	return b.stateStore().saveProjectBackgroundOperation(operation)
}
func (b *localProjectBackground) projectBackgroundContainer(ctx context.Context) (string, error) {
	out, err := b.provisioner.runIntegratedPlatformComposeOutput(ctx, "ps", "-q", "sla-notifier")
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(out))
	if len(ids) > 1 {
		return "", errors.New("multiple local project background consumers")
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}
func (b *localProjectBackground) projectBackgroundInspect(ctx context.Context, id, format string) (string, error) {
	// The common lifecycle asks for image identity. For local pins that identity
	// is .Image (config digest); .Config.Image can remain a mutable build tag.
	if format == "{{.Config.Image}}" {
		format = "{{.Image}}"
	}
	runner, ok := b.provisioner.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return "", errors.New("local background inspection output unavailable")
	}
	out, err := runner.RunOutput(ctx, b.provisioner.localPlatformRoot(), os.Environ(), b.provisioner.config.DockerBinary, "inspect", "--format", format, id)
	if err != nil || len(out) > 4096 {
		return "", errors.New("local background inspection failed")
	}
	return strings.TrimSpace(string(out)), nil
}
func (b *localProjectBackground) projectBackgroundControl(ctx context.Context, action, operation, build string, drained bool) (json.RawMessage, error) {
	out, err := b.provisioner.runIntegratedPlatformComposeOutput(ctx, "exec", "-T", "sla-notifier", "/usr/local/bin/project-background-control", "--action", action, "--operation-id", operation)
	if err != nil {
		return nil, fmt.Errorf("local project background %s failed; operation retained", action)
	}
	if err := validateProjectBackgroundControl(out, build, b.tenant, b.environment, action, drained); err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
func (b *localProjectBackground) preflight(ctx context.Context) error {
	_, build, err := b.projectBackgroundRelease()
	if err != nil {
		return err
	}
	out, err := b.provisioner.runIntegratedPlatformComposeOutput(ctx, "config", "--format", "json", "project-api", "sla-notifier")
	if err != nil {
		return errors.New("local project Compose preflight unavailable")
	}
	var model struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
			DependsOn   map[string]struct {
				Condition string `json:"condition"`
			} `json:"depends_on"`
			Healthcheck struct {
				Test    []string `json:"test"`
				Disable bool     `json:"disable"`
			} `json:"healthcheck"`
		} `json:"services"`
	}
	if len(out) > 4194304 || json.Unmarshal(out, &model) != nil {
		return errors.New("local project Compose projection invalid")
	}
	for _, name := range []string{"project-api", "sla-notifier"} {
		if model.Services[name].Environment["PROJECT_BACKGROUND_BUILD_ID"] != build {
			return errors.New("local project Compose image/build mismatch")
		}
	}
	worker := model.Services["sla-notifier"]
	if worker.Environment["TEMPORAL_ADDRESS"] == "" || worker.Environment["TEMPORAL_NAMESPACE"] == "" || worker.DependsOn["temporal"].Condition != "service_healthy" || worker.Healthcheck.Disable || !strings.Contains(strings.Join(worker.Healthcheck.Test, " "), "/healthz") {
		return errors.New("local project lacks Temporal/bootstrap health contract; merge current compose.local.yaml project fields")
	}
	return nil
}

func (b *localProjectBackground) stopProjectBackground(ctx context.Context) error {
	return b.provisioner.runIntegratedPlatformCompose(ctx, "stop", "--timeout", "300", "sla-notifier")
}
