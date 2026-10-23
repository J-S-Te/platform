package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type projectBackgroundFixtureRunner struct {
	calls                     []string
	image, marker, failAction string
	undrained, notReady       bool
	absent                    bool
	engine, paused            string
	stopError                 bool
	notBootstrap              bool
}

func (r *projectBackgroundFixtureRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) error {
	r.calls = append(r.calls, strings.Join(args, " "))
	if r.stopError && containsString(args, "stop") {
		return errors.New("stop failed")
	}
	return nil
}

func (r *projectBackgroundFixtureRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if containsString(args, "--services") {
		return []byte("project-api\nproject-sla-notifier\nproject-migrate\nproject-mysql\n"), nil
	}
	if containsString(args, "ps") {
		if r.absent {
			return nil, nil
		}
		return []byte("qa-project-container\n"), nil
	}
	for _, arg := range args {
		switch arg {
		case "{{.Config.Image}}":
			return []byte(r.image), nil
		case `{{index .Config.Labels "io.uip.project-background.protocol"}}`:
			return []byte(r.marker), nil
		case "{{.Path}}":
			return []byte("unknown-worker"), nil
		}
	}
	action := ""
	for i, arg := range args {
		if arg == "--action" && i+1 < len(args) {
			action = args[i+1]
		}
	}
	if action == "" {
		if containsString(args, "run") || containsString(args, "up") || containsString(args, "exec") {
			return nil, nil
		}
		return nil, errors.New("unexpected fixture command")
	}
	if action == r.failAction {
		return nil, errors.New("control unavailable")
	}
	return json.Marshal(map[string]any{
		"protocol": 1, "engine": r.engine, "paused_reason": r.paused, "tenant_id": "qa-tenant", "environment": "qa",
		"build_id": "project-background-sha256-" + strings.Repeat("a", 64), "bootstrap_ready": !r.notBootstrap,
		"routing_ready": !r.notReady || r.paused == "PAUSED_OPERATOR", "execution_ready": !r.notReady, "active_activities": 0,
		"active_leases": 0, "drained": !r.undrained, "schedules": []any{},
	})
}

func projectBackgroundTargetFixture(t *testing.T) (*productionComposeTarget, *projectBackgroundFixtureRunner) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	image := "qa.invalid/project@sha256:" + strings.Repeat("a", 64)
	release := filepath.Join(root, ".release.env")
	if err := os.WriteFile(release, []byte("PROJECT_IMAGE="+image+"\nPROJECT_BACKGROUND_BUILD_ID=project-background-sha256-"+strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &projectBackgroundFixtureRunner{image: image, marker: "1", engine: "temporal"}
	target := &productionComposeTarget{runner: runner}
	target.config.DeployRoot, target.config.ReleaseEnvPath = root, release
	target.config.AllowedTenantID = "qa-tenant"
	target.config.DockerBinary = "docker"
	target.config.ComposeFile = filepath.Join(root, "docker-compose.yml")
	content, err := os.ReadFile(filepath.Join(platformModuleRoot(t), "deploy", "production", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.config.ComposeFile, content, 0600); err != nil {
		t.Fatal(err)
	}
	target.config.Profile.Manifest.Application.Code = "project_management"
	target.config.Profile.Manifest.Application.Environment = "qa"
	return target, runner
}

func TestProductionProjectBackgroundPrepareFailsClosedWithoutControl(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.failAction = "inspect"
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("failed control accepted as legacy")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "stop") {
			t.Fatal("old worker stopped without evidence")
		}
	}
}

func TestProductionProjectBackgroundDrainMustProveNoPendingWork(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.undrained = true
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("incomplete drain accepted")
	}
	data, err := os.ReadFile(filepath.Join(target.config.DeployRoot, "runtime", "project-background", "operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil || state["stage"] != "drain-pending" {
		t.Fatal("failed drain lost resumable operation")
	}
}

func TestProductionProjectBackgroundResumeRequiresFreshDrainEvidence(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.failAction = "drain"
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("resume trusted a saved phase instead of current drain evidence")
	}
}

func TestProductionProjectBackgroundActivationChecksReadinessAfterControlSuccess(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.notReady = true
	if err := target.activateProjectBackground(context.Background()); err == nil {
		t.Fatal("zero exit alone reported ready")
	}
}

func TestProductionProjectBackgroundOrderAndImageBuildBinding(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := target.activateProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, action := range []string{"inspect", "pause", "drain", "inspect", "ensure", "resume", "verify"} {
		found := -1
		for i := last + 1; i < len(runner.calls); i++ {
			if strings.Contains(runner.calls[i], "--action "+action+" ") {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("missing ordered action %s", action)
		}
		last = found
	}
	if err := os.WriteFile(target.config.ReleaseEnvPath, []byte("PROJECT_IMAGE="+runner.image+"\nPROJECT_BACKGROUND_BUILD_ID=placeholder\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.activateProjectBackground(context.Background()); err == nil {
		t.Fatal("mismatched build accepted")
	}
}

func TestProductionProjectBackgroundFailedDrainBlocksRealDeployPath(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.failAction = "drain"
	target.config.Profile.Manifest.Compose.RuntimeServices = []string{"project-api", "project-sla-notifier"}
	target.config.Profile.Manifest.Compose.MigrateService = "project-migrate"
	if err := target.deployLocked(context.Background()); err == nil {
		t.Fatal("deployment ignored failed drain")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "run --name") || strings.Contains(call, "--force-recreate") {
			t.Fatalf("deployment changed services before drain: %s", call)
		}
	}
}

func TestProductionProjectBackgroundProtocolLegacyPauseBlocksReplacement(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.engine = "legacy"
	runner.paused = "PAUSED_OPERATOR"
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("manual legacy pause lost on replacement")
	}
	op, err := target.loadProjectBackgroundOperation()
	if err != nil {
		t.Fatal(err)
	}
	if op.Engine != "legacy" {
		t.Fatalf("actual engine lost: %s", op.Engine)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "--action pause ") {
			t.Fatal("paused before blocking unsafe replacement")
		}
	}
}
func TestProductionProjectBackgroundControlledStopRetainsPriorOnReleaseChange(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err := target.loadProjectBackgroundOperation()
	if err != nil {
		t.Fatal(err)
	}
	oldOperation := op.OperationID
	op.Stage = "stopped"
	if err := target.saveProjectBackgroundOperation(op); err != nil {
		t.Fatal(err)
	}
	runner.absent = true
	image := "qa.invalid/project@sha256:" + strings.Repeat("b", 64)
	if err := os.WriteFile(target.config.ReleaseEnvPath, []byte("PROJECT_IMAGE="+image+"\nPROJECT_BACKGROUND_BUILD_ID=project-background-sha256-"+strings.Repeat("b", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err = target.loadProjectBackgroundOperation()
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationID != oldOperation || op.OldImage != runner.image || len(op.Prior) == 0 {
		t.Fatal("controlled stop lost original operation/image/pause metadata")
	}
}
func TestProductionProjectBackgroundFirstInstallAndUnexpectedMissing(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.absent = true
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ := target.loadProjectBackgroundOperation()
	if op.Engine != "none" || op.Stage != "prepared" {
		t.Fatal("first install not prepared")
	}
	runner.absent = false
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.absent = true
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("unexpected missing worker accepted")
	}
}
func TestProductionProjectBackgroundLicensePauseRetainsPendingOperation(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.notReady = true
	runner.paused = "PAUSED_LICENSE"
	if err := target.activateProjectBackground(context.Background()); err == nil {
		t.Fatal("license pause accepted")
	}
	op, _ := target.loadProjectBackgroundOperation()
	if op.Stage != "activating" || op.OperationID == "" {
		t.Fatal("pending operation lost")
	}
}

func TestProductionProjectBackgroundFullDeployOrdering(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	target.config.Profile.Manifest.Compose.RuntimeServices = []string{"project-api", "project-sla-notifier"}
	target.config.Profile.Manifest.Compose.MigrateService = "project-migrate"
	target.config.Profile.Manifest.Compose.Database = &productionSubsystemDatabaseManifest{Service: "project-mysql", Name: "project"}
	if err := target.deployLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, step := range []string{"exec -T project-mysql", "--action pause ", "--action drain ", "run --name", "--force-recreate", "--action inspect ", "--action ensure ", "--action resume ", "--action verify "} {
		found := -1
		for i := last + 1; i < len(runner.calls); i++ {
			if strings.Contains(runner.calls[i], step) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("missing ordered deploy step %s: %v", step, runner.calls)
		}
		last = found
	}
}
func TestProductionProjectBackgroundOldComposePreflightBeforeControl(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := os.WriteFile(target.config.ComposeFile, []byte("services: {project-sla-notifier: {image: pending}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.prepareProjectBackground(context.Background()); err == nil || !strings.Contains(err.Error(), ".dist") {
		t.Fatalf("old Compose accepted or unsafe diagnostics: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("old Compose paused running service")
	}
}
func TestProductionProjectBackgroundTemporalOperatorPauseSurvivesUpgrade(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.paused = "PAUSED_OPERATOR"
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.notReady = true
	if err := target.activateProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ := target.loadProjectBackgroundOperation()
	if op.Stage != "complete" || !strings.Contains(string(op.Prior), "PAUSED_OPERATOR") {
		t.Fatal("original pause metadata lost")
	}
}
func TestProductionProjectBackgroundPointerRollbackBeforeNewDrainRejected(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := len(runner.calls)
	image := "qa.invalid/project@sha256:" + strings.Repeat("b", 64)
	if err := os.WriteFile(target.config.ReleaseEnvPath, []byte("PROJECT_IMAGE="+image+"\nPROJECT_BACKGROUND_BUILD_ID=project-background-sha256-"+strings.Repeat("b", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("guessed release restoration before new build drain")
	}
	if len(runner.calls) != count {
		t.Fatal("pointer conflict touched current worker")
	}
}
func TestProductionProjectBackgroundLegacyPendingOperationBlocksReplacement(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	runner.engine = "legacy"
	runner.paused = "PAUSED_OPERATION"
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("original legacy pending operation lost")
	}
}

func TestProductionProjectBackgroundTeardownPersistsProofOnlyAfterActualStop(t *testing.T) {
	for _, failStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failStop], func(t *testing.T) {
			target, runner := projectBackgroundTargetFixture(t)
			runner.stopError = failStop
			target.config.Timeout = 5 * time.Second
			target.config.RuntimeEnvPath = filepath.Join(target.config.DeployRoot, ".env")
			if err := os.WriteFile(target.config.RuntimeEnvPath, []byte("COMPOSE_PROJECT_NAME=qa-project\n"), 0600); err != nil {
				t.Fatal(err)
			}
			target.config.Profile.Manifest.Compose.TeardownServices = []string{"project-api", "project-sla-notifier"}
			err := target.Teardown(context.Background(), "qa-tenant")
			if (err != nil) != failStop {
				t.Fatalf("teardown err=%v failure=%v", err, failStop)
			}
			op, err := target.loadProjectBackgroundOperation()
			if err != nil {
				t.Fatal(err)
			}
			if failStop {
				if op.Stage == "stopped" {
					t.Fatal("failed stop wrote false proof")
				}
			} else {
				if op.Stage != "stopped" || op.OldImage != runner.image || len(op.Prior) == 0 {
					t.Fatal("actual stop lost proof or prior metadata")
				}
				runner.absent = true
				if err := target.prepareProjectBackground(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := target.prepareProjectBackground(context.Background()); err != nil {
					t.Fatal("stopped proof lost on retry:", err)
				}
			}
		})
	}
}

func TestProductionProjectBackgroundComposeMustBindExactReleaseBuild(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	content, err := os.ReadFile(target.config.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.ReplaceAll(string(content), "${PROJECT_BACKGROUND_BUILD_ID:-}", "${PROJECT_BACKGROUND_BUILD_ID_WRONG:-}"))
	if err := os.WriteFile(target.config.ComposeFile, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.prepareProjectBackground(context.Background()); err == nil {
		t.Fatal("Compose accepted a different Build variable")
	}
	if len(runner.calls) != 0 {
		t.Fatal("invalid Build binding touched running consumer")
	}
}

func TestProductionProjectBackgroundBootstrapBeforePromotion(t *testing.T) {
	target, runner := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.notBootstrap = true
	if err := target.activateProjectBackground(context.Background()); err == nil {
		t.Fatal("unbootstrapped candidate promoted")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "--action ensure ") {
			t.Fatal("promotion occurred before bootstrap")
		}
	}
}

func TestProductionProjectBackgroundFirstFailureRetainsNoOldRuntimeAndResumes(t *testing.T) {
	for _, failure := range []string{"license", "bootstrap", "dependency"} {
		t.Run(failure, func(t *testing.T) {
			target, runner := projectBackgroundTargetFixture(t)
			runner.absent = true
			if err := target.prepareProjectBackground(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, err := target.loadProjectBackgroundOperation()
			if err != nil {
				t.Fatal(err)
			}
			runner.absent = false
			runner.notReady = failure == "license"
			runner.notBootstrap = failure == "bootstrap"
			if failure == "license" {
				runner.paused = "PAUSED_LICENSE"
			}
			if failure == "dependency" {
				runner.failAction = "verify"
			}
			if err := target.activateProjectBackground(context.Background()); err == nil {
				t.Fatal("license failure accepted")
			}
			op, err := target.loadProjectBackgroundOperation()
			if err != nil {
				t.Fatal(err)
			}
			if op.OldImage != "" || op.OldBuildID != "" || len(op.Prior) != 0 && string(op.Prior) != "null" {
				t.Fatal("first candidate was misclassified as rollback runtime")
			}
			if op.OperationID != before.OperationID || op.ReleaseImage != runner.image || op.CurrentImage != runner.image || op.CurrentEngine != "temporal" || (failure != "bootstrap" && op.Stage != "activating") {
				t.Fatal("failure lost candidate/operation alignment")
			}
			runner.absent = true
			if err := target.prepareProjectBackground(context.Background()); err == nil {
				t.Fatal("observed first-install candidate disappeared without stopped proof")
			}
			runner.absent = false
			runner.notReady = false
			runner.notBootstrap = false
			runner.failAction = ""
			runner.paused = ""
			if err := target.prepareProjectBackground(context.Background()); err != nil {
				t.Fatal(err)
			}
			op, err = target.loadProjectBackgroundOperation()
			if err != nil {
				t.Fatal(err)
			}
			if op.OldImage != "" || op.OldBuildID != "" || len(op.Prior) != 0 && string(op.Prior) != "null" {
				t.Fatal("resume reclassified first candidate as old runtime")
			}
			if err := target.activateProjectBackground(context.Background()); err != nil {
				t.Fatal(err)
			}
			op, _ = target.loadProjectBackgroundOperation()
			if op.Stage != "complete" || op.OperationID != before.OperationID {
				t.Fatal("same candidate did not resume")
			}
			runner.absent = true
			if err := target.prepareProjectBackground(context.Background()); err == nil {
				t.Fatal("complete first install lost observed runtime existence")
			}

		})
	}
}
