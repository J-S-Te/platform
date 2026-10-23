package infrastructure

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// The Agent uses the same fixed control protocol as the host helper. It does
// not execute host scripts or add a Python/shell dependency to its trust boundary.
type projectBackgroundOperation struct {
	Protocol       int             `json:"protocol"`
	OperationID    string          `json:"operation_id"`
	ReleaseImage   string          `json:"release_image"`
	OldImage       string          `json:"old_image"`
	CurrentImage   string          `json:"current_image,omitempty"`
	CurrentBuildID string          `json:"current_build_id,omitempty"`
	CurrentEngine  string          `json:"current_engine,omitempty"`
	OldBuildID     string          `json:"old_build_id"`
	Engine         string          `json:"engine"`
	Stage          string          `json:"stage"`
	Prior          json.RawMessage `json:"prior"`
}

type projectBackgroundControlState struct {
	Protocol       int    `json:"protocol"`
	Engine         string `json:"engine"`
	TenantID       string `json:"tenant_id"`
	Environment    string `json:"environment"`
	BuildID        string `json:"build_id"`
	BootstrapReady bool   `json:"bootstrap_ready"`
	RoutingReady   bool   `json:"routing_ready"`
	ExecutionReady bool   `json:"execution_ready"`
	PausedReason   string `json:"paused_reason"`
	Schedules      []struct {
		OperationID string `json:"operation_id"`
	} `json:"schedules"`
	ActiveActivities *int `json:"active_activities"`
	ActiveLeases     *int `json:"active_leases"`
	Drained          bool `json:"drained"`
}

func projectBackgroundBuildID(image string) (string, error) {
	if !isImmutableProductionImageReference(image) {
		return "", errors.New("project background requires immutable image")
	}
	return "project-background-sha256-" + image[strings.LastIndex(image, "@sha256:")+len("@sha256:"):], nil
}

func (target *productionComposeTarget) projectBackgroundRelease() (string, string, error) {
	content, err := os.ReadFile(target.config.ReleaseEnvPath)
	if err != nil {
		return "", "", err
	}
	values := parseEnvironmentValues(string(content))
	image := values["PROJECT_IMAGE"]
	build, err := projectBackgroundBuildID(image)
	if err != nil {
		return "", "", err
	}
	if values["PROJECT_BACKGROUND_BUILD_ID"] != build {
		return "", "", errors.New("project background image/build pointers do not match")
	}
	return image, build, nil
}

func (target *productionComposeTarget) projectBackgroundStatePath() (string, error) {
	directory := filepath.Join(target.config.DeployRoot, "runtime", "project-background")
	if err := ensureProductionRuntimeDirectory(target.config.DeployRoot, directory, true); err != nil {
		return "", err
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("project background state directory must be private")
	}
	return filepath.Join(directory, "operation.json"), nil
}

func (target *productionComposeTarget) loadProjectBackgroundOperation() (*projectBackgroundOperation, error) {
	path, err := target.projectBackgroundStatePath()
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 262144 {
		return nil, errors.New("unsafe project background operation")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var operation projectBackgroundOperation
	if err := json.Unmarshal(content, &operation); err != nil || operation.Protocol != 1 || operation.OperationID == "" {
		return nil, errors.New("invalid project background operation")
	}
	return &operation, nil
}

func (target *productionComposeTarget) saveProjectBackgroundOperation(operation *projectBackgroundOperation) error {
	path, err := target.projectBackgroundStatePath()
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return errors.New("unsafe project background operation")
	}
	content, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".operation-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(content, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Keep recovery usable by the host deployment account as well as uid0 Agent.
	parent, err := os.Stat(filepath.Join(target.config.DeployRoot, "runtime"))
	if err != nil {
		return err
	}
	if owner, ok := parent.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := os.Chown(filepath.Dir(path), int(owner.Uid), int(owner.Gid)); err != nil {
			return err
		}
		if err := os.Chown(file.Name(), int(owner.Uid), int(owner.Gid)); err != nil {
			return err
		}
	}
	return os.Rename(file.Name(), path)
}

func (target *productionComposeTarget) projectBackgroundContainer(ctx context.Context) (string, error) {
	output, err := target.runComposeOutput(ctx, "ps", "-q", "project-sla-notifier")
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(output))
	if len(ids) > 1 {
		return "", errors.New("multiple project background consumers")
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

func (target *productionComposeTarget) projectBackgroundInspect(ctx context.Context, id, format string) (string, error) {
	runner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return "", errors.New("project background inspection output unavailable")
	}
	output, err := runner.RunOutput(ctx, target.config.DeployRoot, os.Environ(), target.config.DockerBinary, "inspect", "--format", format, id)
	return strings.TrimSpace(string(output)), err
}

func (target *productionComposeTarget) projectBackgroundControl(ctx context.Context, action, operation, build string, drained bool) (json.RawMessage, error) {
	output, err := target.runComposeOutput(ctx, "exec", "-T", "project-sla-notifier", "/usr/local/bin/project-background-control", "--action", action, "--operation-id", operation)
	if err != nil {
		return nil, fmt.Errorf("project background %s failed; operation retained", action)
	}
	if err := validateProjectBackgroundControl(output, build, target.config.AllowedTenantID, target.config.Profile.Manifest.Application.Environment, action, drained); err != nil {
		return nil, err
	}
	return json.RawMessage(output), nil
}

func validateProjectBackgroundControl(output []byte, build, tenant, environment, action string, drained bool) error {
	var state projectBackgroundControlState
	if len(output) > 262144 || json.Unmarshal(output, &state) != nil || state.Protocol != 1 || tenant == "" || environment == "" || state.BuildID != build || state.TenantID != tenant || state.Environment != environment || state.Engine != "temporal" && state.Engine != "legacy" {
		return errors.New("project background control identity mismatch")
	}
	if drained && (!state.Drained || state.ActiveActivities == nil || *state.ActiveActivities != 0 || state.ActiveLeases == nil || *state.ActiveLeases != 0) {
		return errors.New("project background drain evidence incomplete")
	}
	if action == "verify" && (!state.BootstrapReady || !state.RoutingReady || !state.ExecutionReady && state.PausedReason != "PAUSED_OPERATOR") {
		return errors.New("project background routing/execution remains pending")
	}
	return nil
}

// Both production and local adapters retain their own image identity and execution boundary.
type projectBackgroundBackend interface {
	projectBackgroundRelease() (string, string, error)
	loadProjectBackgroundOperation() (*projectBackgroundOperation, error)
	saveProjectBackgroundOperation(*projectBackgroundOperation) error
	projectBackgroundContainer(context.Context) (string, error)
	projectBackgroundInspect(context.Context, string, string) (string, error)
	projectBackgroundControl(context.Context, string, string, string, bool) (json.RawMessage, error)
	stopProjectBackground(context.Context) error
	projectBackgroundImageBuildID(string) (string, error)
}

func (target *productionComposeTarget) projectBackgroundImageBuildID(image string) (string, error) {
	return projectBackgroundBuildID(image)
}
func (target *productionComposeTarget) stopProjectBackground(ctx context.Context) error {
	return target.runCompose(ctx, "stop", "--timeout", "300", "project-sla-notifier")
}

func projectBackgroundReleaseBinding(value, key string) bool {
	return value == "${"+key+"}" || strings.HasPrefix(value, "${"+key+":-") && strings.HasSuffix(value, "}") && strings.Count(value, "${") == 1
}

func (target *productionComposeTarget) preflightProjectBackground() error {
	content, err := os.ReadFile(target.config.ComposeFile)
	if err != nil {
		return errors.New("project Compose preflight unavailable; merge docker-compose.yml.dist project fields")
	}
	var document struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			DependsOn   map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
			Healthcheck struct {
				Test    []string `yaml:"test"`
				Disable bool     `yaml:"disable"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if len(content) > 4194304 || yaml.Unmarshal(content, &document) != nil {
		return errors.New("project Compose preflight invalid; merge docker-compose.yml.dist project fields")
	}
	service, ok := document.Services["project-sla-notifier"]
	test := strings.Join(service.Healthcheck.Test, " ")
	if !ok || !projectBackgroundReleaseBinding(service.Image, "PROJECT_IMAGE") || !projectBackgroundReleaseBinding(service.Environment["PROJECT_BACKGROUND_BUILD_ID"], "PROJECT_BACKGROUND_BUILD_ID") || service.Environment["TEMPORAL_ADDRESS"] == "" || service.Environment["TEMPORAL_NAMESPACE"] == "" || service.DependsOn["temporal"].Condition != "service_healthy" || service.Healthcheck.Disable || !strings.Contains(test, "/healthz") || strings.Contains(test, "/readyz") {
		return errors.New("project Compose lacks Build/Temporal/bootstrap health contract; merge docker-compose.yml.dist project fields before retry")
	}
	return nil
}

func legacyProjectBackgroundReplacementGuard(prior json.RawMessage) error {
	if len(prior) == 0 || string(prior) == "null" {
		return nil
	}
	var state projectBackgroundControlState
	if json.Unmarshal(prior, &state) != nil {
		return errors.New("invalid original project background state")
	}
	if state.Engine == "legacy" {
		pending := state.PausedReason == "PAUSED_OPERATOR" || state.PausedReason == "PAUSED_DEPLOYMENT" || state.PausedReason == "PAUSED_OPERATION"
		for _, schedule := range state.Schedules {
			pending = pending || schedule.OperationID != ""
		}
		if pending {
			return errors.New("legacy operator pause or pending operation cannot survive process replacement; complete/resume in original process under controlled operation, then prepare again or migrate to Temporal")
		}
	}
	return nil
}

func (target *productionComposeTarget) prepareProjectBackground(ctx context.Context) error {
	if target.config.Profile.Manifest.Application.Code != "project_management" {
		return nil
	}
	if err := target.preflightProjectBackground(); err != nil {
		return err
	}
	return prepareProjectBackground(ctx, target)
}

func prepareProjectBackground(ctx context.Context, target projectBackgroundBackend, stopping ...bool) error {
	forStop := len(stopping) > 0 && stopping[0]
	image, _, err := target.projectBackgroundRelease()
	if err != nil {
		return err
	}
	operation, err := target.loadProjectBackgroundOperation()
	if err != nil {
		return err
	}
	if operation == nil || operation.Stage == "complete" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		previous := operation
		operation = &projectBackgroundOperation{Protocol: 1, OperationID: "project-" + hex.EncodeToString(token[:]), ReleaseImage: image, Stage: "inspect", Engine: "none"}
		if previous != nil {
			operation.Engine = previous.Engine
			if previous.CurrentEngine != "" {
				operation.Engine = previous.CurrentEngine
			}
		}
		if err := target.saveProjectBackgroundOperation(operation); err != nil {
			return err
		}
	} else if operation.Stage == "stopped" {
		operation.ReleaseImage = image
	} else if operation.ReleaseImage != image {
		return errors.New("unfinished project background operation conflicts with release image")
	}
	id, err := target.projectBackgroundContainer(ctx)
	if err != nil {
		return err
	}
	if !forStop {
		if err := legacyProjectBackgroundReplacementGuard(operation.Prior); err != nil {
			return err
		}
	}
	if id == "" && (operation.Engine != "none" || operation.CurrentImage != "") && operation.Stage != "stopped" {
		return errors.New("previous project background is missing; fresh drain evidence is unavailable")
	}
	if id != "" {
		oldImage, err := target.projectBackgroundInspect(ctx, id, "{{.Config.Image}}")
		if err != nil {
			return err
		}
		build, err := target.projectBackgroundImageBuildID(oldImage)
		if err != nil {
			return err
		}
		marker, err := target.projectBackgroundInspect(ctx, id, `{{index .Config.Labels "io.uip.project-background.protocol"}}`)
		if err != nil {
			return err
		}
		if marker == "1" {
			prior, err := target.projectBackgroundControl(ctx, "inspect", operation.OperationID, build, false)
			if err != nil {
				return err
			}
			var state projectBackgroundControlState
			if err := json.Unmarshal(prior, &state); err != nil {
				return err
			}
			operation.CurrentImage, operation.CurrentBuildID, operation.CurrentEngine = oldImage, build, state.Engine
			if operation.Stage == "inspect" {
				operation.OldImage, operation.OldBuildID, operation.Engine, operation.Prior = oldImage, build, state.Engine, prior
				if err := target.saveProjectBackgroundOperation(operation); err != nil {
					return err
				}
			}
			if !forStop {
				if err := legacyProjectBackgroundReplacementGuard(operation.Prior); err != nil {
					return err
				}
			}
			operation.Stage = "drain-pending"
			if err := target.saveProjectBackgroundOperation(operation); err != nil {
				return err
			}
			if _, err := target.projectBackgroundControl(ctx, "pause", operation.OperationID, build, false); err != nil {
				return err
			}
			if _, err := target.projectBackgroundControl(ctx, "drain", operation.OperationID, build, true); err != nil {
				return err
			}
		} else if marker == "" || marker == "<no value>" {
			identity, err := target.projectBackgroundInspect(ctx, id, "{{.Path}}")
			if err != nil {
				return err
			}
			if identity != "/usr/local/bin/sla-notifier" && identity != "sla-notifier" && identity != "./sla-notifier" {
				return errors.New("unknown project worker cannot be treated as legacy")
			}
			operation.CurrentImage, operation.CurrentBuildID, operation.CurrentEngine = oldImage, build, "legacy"
			if operation.Stage == "inspect" {
				operation.OldImage, operation.OldBuildID, operation.Engine = oldImage, build, "legacy"
			}
			operation.Stage = "drain-pending"
			if err := target.saveProjectBackgroundOperation(operation); err != nil {
				return err
			}
			if err := target.stopProjectBackground(ctx); err != nil {
				return err
			}
			operation.Stage = "stopped"
			return target.saveProjectBackgroundOperation(operation)
		} else {
			return errors.New("unknown project background control protocol")
		}
	}
	if operation.Stage != "stopped" {
		operation.Stage = "prepared"
	}
	return target.saveProjectBackgroundOperation(operation)
}

func (target *productionComposeTarget) activateProjectBackground(ctx context.Context) error {
	if target.config.Profile.Manifest.Application.Code != "project_management" {
		return nil
	}
	return activateProjectBackground(ctx, target)
}

func activateProjectBackground(ctx context.Context, target projectBackgroundBackend) error {
	image, build, err := target.projectBackgroundRelease()
	if err != nil {
		return err
	}
	operation, err := target.loadProjectBackgroundOperation()
	if err != nil {
		return err
	}
	if operation == nil || operation.ReleaseImage != image {
		return errors.New("project background operation does not match current release")
	}
	id, err := target.projectBackgroundContainer(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("project background consumer is missing")
	}
	actual, err := target.projectBackgroundInspect(ctx, id, "{{.Config.Image}}")
	if err != nil {
		return err
	}
	if actual != image {
		return errors.New("project background image mismatch")
	}
	prior, err := target.projectBackgroundControl(ctx, "inspect", operation.OperationID, build, false)
	if err != nil {
		return err
	}
	var state projectBackgroundControlState
	if err := json.Unmarshal(prior, &state); err != nil {
		return err
	}
	// Observing a candidate proves its presence, not a pre-operation rollback runtime.
	operation.CurrentImage, operation.CurrentBuildID, operation.CurrentEngine = image, build, state.Engine
	if err := target.saveProjectBackgroundOperation(operation); err != nil {
		return err
	}
	if !state.BootstrapReady {
		return errors.New("project background bootstrap is pending; promotion prohibited")
	}
	operation.Stage = "activating"
	if err := target.saveProjectBackgroundOperation(operation); err != nil {
		return err
	}
	for _, action := range []string{"ensure", "resume", "verify"} {
		if _, err := target.projectBackgroundControl(ctx, action, operation.OperationID, build, false); err != nil {
			return err
		}
	}
	operation.Stage = "complete"
	return target.saveProjectBackgroundOperation(operation)
}

// Mark only after the caller successfully stops/removes the protected service.
func markProjectBackgroundStopped(target projectBackgroundBackend) error {
	op, err := target.loadProjectBackgroundOperation()
	if err != nil || op == nil {
		return err
	}
	if op.Stage == "stopped" {
		return nil
	}
	if op.Stage != "prepared" && op.Stage != "teardown-drained" {
		return errors.New("project background stop lacks prepared operation")
	}
	op.Stage = "stopped"
	return target.saveProjectBackgroundOperation(op)
}
