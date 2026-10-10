package coordination

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/migrations"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type testSigner struct {
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	vendor map[string]ed25519.PublicKey
}

func (s testSigner) Sign(p runtime.PlatformSnapshot) (string, error) {
	return runtime.SignSnapshot(p, "test-platform", s.priv)
}
func (s testSigner) Verify(raw string, b runtime.Binding) (runtime.PlatformSnapshot, error) {
	return runtime.VerifySnapshot(raw, b, map[string]ed25519.PublicKey{"test-platform": s.pub}, s.vendor)
}
func TestMySQLCoordinationLifecycle(t *testing.T) {
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("LICENSE_TEST_DSN is not configured")
	}
	cfg, e := driver.ParseDSN(dsn)
	if e != nil {
		t.Fatal("invalid test DSN")
	}
	host, _, e := net.SplitHostPort(cfg.Addr)
	if e != nil || cfg.Net != "tcp" || (host != "127.0.0.1" && host != "localhost") || cfg.DBName != "platform_license_test" {
		t.Fatal("isolated loopback platform_license_test required")
	}
	db, e := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := sql.Close(); e != nil {
			t.Error(e)
		}
	})
	ctx := context.Background()
	if _, e = migration.Run(ctx, db, migrations.Files); e != nil {
		t.Fatal(e)
	}
	now := time.Unix(1900000000, 0)
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	spub, spriv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	keys := map[string]ed25519.PublicKey{"test-vendor": pub}
	repo, e := infrastructure.NewRepository(db)
	if e != nil {
		t.Fatal(e)
	}
	lic, e := application.NewService(repo, keys, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	actor := domain.Actor{TenantID: "coord-test-tenant", UserID: "coord-test-user"}
	state, e := lic.Initialize(ctx, application.InitializeInput{CustomerID: "coord-test-customer", Environment: "production"}, actor)
	if e != nil {
		t.Fatal(e)
	}
	artifactDigest := ""
	cleanup := func() {
		if e := db.Where("application = ?", "contract_management").Delete(&LifecycleEvent{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("service_id IN ?", []string{"coord-contract-api", "coord-contract-worker"}).Delete(&RetiredClient{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("service_id IN ?", []string{"coord-contract-api", "coord-contract-worker", "coord-project-api", "coord-settlement-api"}).Delete(&Member{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("application IN ?", []string{"contract_management", "project_management", "settlement"}).Delete(&Application{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("id = ? AND project = ?", 1, "coord-test").Delete(&Inventory{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("tenant_id = ? OR (kind IN ? AND created_at = ?)", actor.TenantID, []string{"ENFORCEMENT_ENFORCED", "RUNTIME_SERVICE_REGISTERED", "MIGRATION_INVENTORY_FROZEN"}, time.Unix(1900000000, 0)).Delete(&domain.Event{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("id = ? AND instance_id = ?", 1, state.InstanceID).Delete(&domain.Deployment{}).Error; e != nil {
			t.Error(e)
		}
		if artifactDigest != "" {
			if e := db.Where("digest = ?", artifactDigest).Delete(&domain.Artifact{}).Error; e != nil {
				t.Error(e)
			}
		}
	}
	t.Cleanup(cleanup)
	s, e := NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	spec := ServiceSpec{Application: "contract_management", Environment: "production", ServiceID: "coord-contract-api", OAuthClientID: "coord-client-api", CoverageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ImageDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	worker := spec
	worker.ServiceID = "coord-contract-worker"
	worker.OAuthClientID = "coord-client-worker"
	report := evidence.Report{Scope: "INSTALLATION", BoundarySupported: true, Project: "coord-test", CollectedAt: now, Complete: true, Facts: []evidence.Fact{}}
	for _, spec := range []ServiceSpec{spec, worker} {
		report.Facts = append(report.Facts, evidence.Fact{Application: spec.Application, Environment: spec.Environment, Service: spec.ServiceID, ContainerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ImageDigest: spec.ImageDigest, Version: "v1", Protocol: "1", Running: true, StartedAt: now.Add(-time.Hour)})
	}
	bad := report
	bad.Scope = "APPLICATION"
	if !errors.Is(s.FreezeInventory(ctx, bad, []ServiceSpec{spec, worker}), ErrNotReady) {
		t.Fatal("single application report accepted for installation")
	}
	bad = report
	bad.BoundarySupported = false
	if !errors.Is(s.FreezeInventory(ctx, bad, []ServiceSpec{spec, worker}), ErrNotReady) {
		t.Fatal("unsupported installation boundary accepted")
	}
	bad = report
	bad.Complete = false
	if !errors.Is(s.FreezeInventory(ctx, bad, []ServiceSpec{spec, worker}), ErrNotReady) {
		t.Fatal("incomplete inventory accepted")
	}
	if e = s.FreezeInventory(ctx, report, []ServiceSpec{spec, worker}); e != nil {
		t.Fatal(e)
	}
	if !errors.Is(s.FreezeInventory(ctx, report, []ServiceSpec{spec, worker}), ErrConflict) {
		t.Fatal("refreeze accepted")
	}
	status, e := s.Status(ctx, spec.Application)
	if e != nil || !status.MigrationEligible {
		t.Fatal("freeze", e)
	}
	machine := func(spec ServiceSpec) context.Context {
		return appctx.WithPrincipal(ctx, appctx.Principal{OAuthClientID: "database-pk-" + spec.ServiceID, ClientID: spec.OAuthClientID, TenantID: "tenant", ApplicationID: "application", ApplicationCode: spec.Application, EnvironmentID: "environment", EnvironmentCode: spec.Environment, Scopes: map[string]struct{}{"license.runtime": {}}})
	}
	if !errors.Is(s.Ready(ctx, spec.ServiceID, ReadyInput{1, spec.CoverageDigest, spec.ImageDigest}), ErrForbidden) {
		t.Fatal("unauthenticated readiness")
	}
	if !errors.Is(s.Ready(machine(worker), spec.ServiceID, ReadyInput{1, spec.CoverageDigest, spec.ImageDigest}), ErrForbidden) {
		t.Fatal("cross client ready")
	}
	if e = s.Ready(machine(spec), spec.ServiceID, ReadyInput{1, spec.CoverageDigest, spec.ImageDigest}); e != nil {
		t.Fatal(e)
	}
	if !errors.Is(s.BeginActivation(ctx, spec.Application, status.Revision, actor), ErrNotReady) {
		t.Fatal("missing worker readiness")
	}
	license := core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "coord-license", Version: 1, CustomerID: state.CustomerID, ProductID: core.Product, Environment: state.Environment, InstanceID: state.InstanceID, IssuedAt: now.Unix(), NotBefore: now.Unix(), Applications: []core.Application{{Code: spec.Application, NotBefore: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(), Kind: "FULL"}}}
	raw, e := core.Sign(license, "test-vendor", priv)
	if e != nil {
		t.Fatal(e)
	}
	preview, e := lic.Preview(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	artifactDigest = preview.Digest
	_, e = lic.Commit(ctx, application.CommitInput{RawJWS: raw, Digest: preview.Digest, ExpectedRevision: preview.Revision, ExpectedCurrentVersion: preview.CurrentVersion, ExpectedPendingDigest: preview.PendingDigest, ConfirmChanges: true}, actor)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Ready(machine(worker), worker.ServiceID, ReadyInput{1, worker.CoverageDigest, worker.ImageDigest}); e != nil {
		t.Fatal(e)
	}
	if e = db.Model(&Member{}).Where("service_id = ?", worker.ServiceID).Update("ready_at", now.Add(-61*time.Second)).Error; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(s.BeginActivation(ctx, spec.Application, status.Revision, actor), ErrNotReady) {
		t.Fatal("stale readiness accepted")
	}
	if e = s.Ready(machine(worker), worker.ServiceID, ReadyInput{1, worker.CoverageDigest, worker.ImageDigest}); e != nil {
		t.Fatal(e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.BeginActivation(ctx, spec.Application, status.Revision, actor); e != nil {
		t.Fatal(e)
	}
	snapshot, e := s.Snapshot(machine(spec), spec.ServiceID)
	if e != nil {
		t.Fatal(e)
	}
	repeat, e := s.Snapshot(machine(spec), spec.ServiceID)
	if e != nil || repeat != snapshot {
		t.Fatal("snapshot not stable", e)
	}
	if !errors.Is(s.Ack(machine(spec), spec.ServiceID, AckInput{snapshot.Revision, "wrong"}), ErrConflict) {
		t.Fatal("wrong ack accepted")
	}
	if e = s.Ack(machine(spec), spec.ServiceID, AckInput{snapshot.Revision, snapshot.Digest}); e != nil {
		t.Fatal(e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Applying {
		t.Fatal("partial ack enforced", e)
	}
	s, e = NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	ws, e := s.Snapshot(machine(worker), worker.ServiceID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Ack(machine(worker), worker.ServiceID, AckInput{ws.Revision, ws.Digest}); e != nil {
		t.Fatal(e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced || status.MigrationEligible {
		t.Fatal("enforcement", e)
	}
	if !errors.Is(s.Ack(machine(spec), spec.ServiceID, AckInput{snapshot.Revision, snapshot.Digest}), ErrConflict) {
		t.Fatal("old ack accepted")
	}
	newSpec := spec
	newSpec.Application = "project_management"
	newSpec.ServiceID = "coord-project-api"
	newSpec.OAuthClientID = "coord-project-client"
	if e = s.Register(ctx, newSpec); e != nil {
		t.Fatal(e)
	}
	newStatus, e := s.Status(ctx, newSpec.Application)
	if e != nil || newStatus.MigrationEligible {
		t.Fatal("new app inherited legacy", e)
	}
	// APP_ENV and OAuth application environments are distinct configuration
	// fields in the real deployment (production versus prod).
	bound, e := NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithApplicationEnvironment("prod"))
	if e != nil {
		t.Fatal(e)
	}
	profileSpec := spec
	profileSpec.Application = "settlement"
	profileSpec.Environment = "prod"
	profileSpec.ServiceID = "coord-settlement-api"
	profileSpec.OAuthClientID = "coord-settlement-client"
	if e = bound.Register(ctx, profileSpec); e != nil {
		t.Fatal("profile registration", e)
	}
	if e = bound.Ready(machine(profileSpec), profileSpec.ServiceID, ReadyInput{runtime.Protocol, profileSpec.CoverageDigest, profileSpec.ImageDigest}); e != nil {
		t.Fatal("profile machine binding", e)
	}
	profileSnapshot, e := bound.Snapshot(machine(profileSpec), profileSpec.ServiceID)
	if e != nil {
		t.Fatal(e)
	}
	verified, e := runtime.VerifySnapshot(profileSnapshot.RawJWS, runtime.Binding{InstanceID: state.InstanceID, Environment: "production", Application: "settlement", ServiceID: profileSpec.ServiceID}, map[string]ed25519.PublicKey{"test-platform": spub}, keys)
	if e != nil || verified.MigrationEligible {
		t.Fatal("deployment license binding changed", e)
	}
	wrongEnvironment := profileSpec
	wrongEnvironment.Environment = "production"
	if !errors.Is(bound.Ready(machine(wrongEnvironment), profileSpec.ServiceID, ReadyInput{runtime.Protocol, profileSpec.CoverageDigest, profileSpec.ImageDigest}), ErrForbidden) {
		t.Fatal("wrong OAuth environment accepted")
	}
	// A reviewed release explicitly replaces API identity and retires the
	// optional Worker. Neither operation may restore migration compatibility.
	oldSpec := spec
	replacement := spec
	replacement.OAuthClientID = "coord-client-api-v2"
	replacement.ImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	approval := fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{replacement}, RequiredServiceIDs: []string{spec.ServiceID}, RetireServiceIDs: []string{worker.ServiceID}}}
	s, e = NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(approval))
	if e != nil {
		t.Fatal(e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil {
		t.Fatal(e)
	}
	release := LifecycleInput{Application: spec.Application, Environment: spec.Environment, ExpectedRevision: status.Revision, OperationID: "coord-reviewed-v2", ReleaseDigest: replacement.ImageDigest}
	unauthorizedRetire, err := NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{replacement}, RequiredServiceIDs: []string{replacement.ServiceID}}}))
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(unauthorizedRetire.ReconcileComponents(ctx, release, actor), ErrForbidden) {
		t.Fatal("unapproved removal accepted")
	}
	if unchanged, err := s.Status(ctx, spec.Application); err != nil || unchanged.Revision != status.Revision || unchanged.State != runtime.Enforced || len(unchanged.Members) != 2 {
		t.Fatal("rejected retirement changed runtime", err)
	}
	for _, outsideID := range []string{profileSpec.ServiceID, "invented-other-app-service"} {
		outsideApproval := fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{replacement}, RequiredServiceIDs: []string{replacement.ServiceID}, RetireServiceIDs: []string{worker.ServiceID, outsideID}}}
		outsideCoordinator, err := NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(outsideApproval))
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(outsideCoordinator.ReconcileComponents(ctx, release, actor), ErrForbidden) {
			t.Fatal("cross-application or invented retirement accepted", outsideID)
		}
		if unchanged, err := s.Status(ctx, spec.Application); err != nil || unchanged.Revision != status.Revision || unchanged.State != runtime.Enforced || len(unchanged.Members) != 2 {
			t.Fatal("outside retirement changed runtime", err)
		}
	}
	type reconciliationResult struct {
		input LifecycleInput
		err   error
	}
	results := make(chan reconciliationResult, 2)
	concurrent := release
	concurrent.OperationID = "coord-reviewed-v2-concurrent"
	for _, input := range []LifecycleInput{release, concurrent} {
		go func(input LifecycleInput) {
			results <- reconciliationResult{input, s.ReconcileComponents(ctx, input, actor)}
		}(input)
	}
	succeeded, conflicted := 0, 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			succeeded++
			release = result.input
		} else if errors.Is(result.err, ErrConflict) {
			conflicted++
		} else {
			t.Fatal("reviewed replacement", result.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", succeeded, conflicted)
	}
	if e = s.ReconcileComponents(ctx, release, actor); e != nil {
		t.Fatal("idempotent replacement", e)
	}
	stale := release
	stale.OperationID = "coord-stale-v2"
	if !errors.Is(s.ReconcileComponents(ctx, stale, actor), ErrConflict) {
		t.Fatal("stale lifecycle accepted")
	}
	if !errors.Is(s.Ready(machine(oldSpec), oldSpec.ServiceID, ReadyInput{1, oldSpec.CoverageDigest, oldSpec.ImageDigest}), ErrForbidden) {
		t.Fatal("old client remained authorized")
	}
	if _, err := s.Snapshot(machine(worker), worker.ServiceID); !errors.Is(err, ErrForbidden) {
		t.Fatal("retired client remained authorized", err)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced || status.MigrationEligible || len(status.Members) != 1 || status.Members[0].AckRevision != 0 || status.Members[0].ReadyAt != nil {
		t.Fatal("replacement inherited ready/ACK/compatibility", e)
	}
	if !errors.Is(s.Ack(machine(replacement), replacement.ServiceID, AckInput{snapshot.Revision, snapshot.Digest}), ErrConflict) {
		t.Fatal("previous acknowledgement inherited")
	}
	if e = s.Ready(machine(replacement), replacement.ServiceID, ReadyInput{1, replacement.CoverageDigest, replacement.ImageDigest}); e != nil {
		t.Fatal(e)
	}
	currentSnapshot, err := s.Snapshot(machine(replacement), replacement.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if e = s.Ack(machine(replacement), replacement.ServiceID, AckInput{currentSnapshot.Revision, currentSnapshot.Digest}); e != nil {
		t.Fatal(e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced {
		t.Fatal("replacement did not finish fresh ACK", e)
	}
	// Recreate the coordinator to exercise persisted idempotence and permanent
	// old-client rejection, not in-memory process state.
	s, e = NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(approval))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ReconcileComponents(ctx, release, actor); e != nil {
		t.Fatal("restart replay", e)
	}
	if !errors.Is(s.Register(ctx, worker), ErrConflict) {
		t.Fatal("retired component resurrected")
	}
	reused := oldSpec
	reused.ServiceID = "coord-contract-worker"
	if !errors.Is(s.Register(ctx, reused), ErrConflict) {
		t.Fatal("retired credential identity reused")
	}
	restored := worker
	restored.OAuthClientID = "coord-client-worker-v3"
	s, e = NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{replacement, restored}, RequiredServiceIDs: []string{replacement.ServiceID, restored.ServiceID}}}))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ReconcileComponents(ctx, LifecycleInput{Application: spec.Application, Environment: spec.Environment, ExpectedRevision: status.Revision, OperationID: "coord-reviewed-restore-v3", ReleaseDigest: restored.ImageDigest}, actor); e != nil {
		t.Fatal("explicit restore", e)
	}
	if _, err = s.Snapshot(machine(worker), worker.ServiceID); !errors.Is(err, ErrForbidden) {
		t.Fatal("restoration reauthorized retired identity", err)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced || status.MigrationEligible || len(status.Members) != 2 {
		t.Fatal("restore membership", e)
	}
	for _, member := range status.Members {
		if member.ServiceID == restored.ServiceID && (member.ReadyAt != nil || member.AckRevision != 0 || member.RetiredAt != nil) {
			t.Fatal("restore inherited readiness/ack")
		}
	}
	for _, memberSpec := range []ServiceSpec{replacement, restored} {
		if e = s.Ready(machine(memberSpec), memberSpec.ServiceID, ReadyInput{1, memberSpec.CoverageDigest, memberSpec.ImageDigest}); e != nil {
			t.Fatal(e)
		}
		snap, err := s.Snapshot(machine(memberSpec), memberSpec.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		if e = s.Ack(machine(memberSpec), memberSpec.ServiceID, AckInput{snap.Revision, snap.Digest}); e != nil {
			t.Fatal(e)
		}
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced {
		t.Fatal("restore did not require complete fresh confirmation", e)
	}
	// An ENFORCED row alone must not report a new license as applied. First
	// establish ACKs for the enforced snapshot, then renew without any polling.
	for _, memberSpec := range []ServiceSpec{replacement, restored} {
		snap, err := s.Snapshot(machine(memberSpec), memberSpec.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Ack(machine(memberSpec), memberSpec.ServiceID, AckInput{snap.Revision, snap.Digest}); err != nil {
			t.Fatal(err)
		}
	}
	// A partial image upgrade must work with an unchanged consumer's durable
	// ENFORCED state. Exercise the actual signed snapshot/file-runtime boundary,
	// rather than ACKing a payload the real consumer would reject as a rollback.
	workerRuntime, err := runtime.NewFileRuntime(filepath.Join(t.TempDir(), "worker-state.json"), runtime.Binding{InstanceID: state.InstanceID, Environment: restored.Environment, Application: restored.Application, ServiceID: restored.ServiceID}, map[string]ed25519.PublicKey{"test-platform": spub}, keys)
	if err != nil {
		t.Fatal(err)
	}
	beforeUpgrade, err := s.Snapshot(machine(restored), restored.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = workerRuntime.ApplySnapshot(ctx, beforeUpgrade.RawJWS, now); err != nil {
		t.Fatal("persist enforced worker snapshot", err)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil {
		t.Fatal(e)
	}
	upgraded := replacement
	upgraded.OAuthClientID = "coord-client-api-v4"
	upgraded.ImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	s, e = NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithLifecycleApprovalVerifier(fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{upgraded, restored}, RequiredServiceIDs: []string{upgraded.ServiceID, restored.ServiceID}}}))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ReconcileComponents(ctx, LifecycleInput{Application: spec.Application, Environment: spec.Environment, ExpectedRevision: status.Revision, OperationID: "coord-reviewed-partial-v4", ReleaseDigest: upgraded.ImageDigest}, actor); e != nil {
		t.Fatal("partial upgrade", e)
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.State != runtime.Enforced || status.MigrationEligible {
		t.Fatal("partial upgrade rolled back enforcement", e)
	}
	for _, member := range status.Members {
		if member.AckRevision != 0 || member.IssuedRevision != 0 || member.RawJWS != "" {
			t.Fatal("partial upgrade inherited current release confirmation")
		}
	}
	if !errors.Is(s.Ack(machine(restored), restored.ServiceID, AckInput{beforeUpgrade.Revision, beforeUpgrade.Digest}), ErrConflict) {
		t.Fatal("unchanged worker old ACK accepted")
	}
	for _, memberSpec := range []ServiceSpec{upgraded, restored} {
		if err = s.Ready(machine(memberSpec), memberSpec.ServiceID, ReadyInput{1, memberSpec.CoverageDigest, memberSpec.ImageDigest}); err != nil {
			t.Fatal(err)
		}
		snap, snapshotErr := s.Snapshot(machine(memberSpec), memberSpec.ServiceID)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if memberSpec.ServiceID == restored.ServiceID {
			if err = workerRuntime.ApplySnapshot(ctx, snap.RawJWS, now); err != nil {
				t.Fatal("unchanged worker rejected controlled partial upgrade", err)
			}
		}
		if err = s.Ack(machine(memberSpec), memberSpec.ServiceID, AckInput{snap.Revision, snap.Digest}); err != nil {
			t.Fatal(err)
		}
	}
	replacement = upgraded
	status, e = s.Status(ctx, spec.Application)
	if e != nil || !status.SnapshotCurrent {
		t.Fatal("current snapshot not recognized", e)
	}
	license.Version++
	license.ID = "coord-license-renewal"
	license.Applications[0].ExpiresAt += 3600
	renewal, err := core.Sign(license, "test-vendor", priv)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := lic.ImportDelivery(ctx, renewal, "production", actor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Where("digest = ?", renewed.State.CurrentDigest).Delete(&domain.Artifact{}).Error; err != nil {
			t.Error(err)
		}
	})
	status, e = s.Status(ctx, spec.Application)
	if e != nil || status.SnapshotCurrent || status.State != runtime.Enforced {
		t.Fatal("old enforcement ACKs confirmed renewal", e)
	}
	for _, memberSpec := range []ServiceSpec{replacement, restored} {
		snap, err := s.Snapshot(machine(memberSpec), memberSpec.ServiceID)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Ack(machine(memberSpec), memberSpec.ServiceID, AckInput{snap.Revision, snap.Digest}); err != nil {
			t.Fatal(err)
		}
	}
	status, e = s.Status(ctx, spec.Application)
	if e != nil || !status.SnapshotCurrent {
		t.Fatal("renewal snapshots not confirmed", e)
	}
	spec = replacement
	now = now.Add(61 * time.Second)
	if !errors.Is(s.Ack(machine(worker), worker.ServiceID, AckInput{ws.Revision, ws.Digest}), ErrForbidden) {
		t.Fatal("old ack after time")
	}
	if e = db.Model(&Member{}).Where("service_id = ?", spec.ServiceID).Updates(map[string]any{"raw_jws": "tampered", "issued_digest": hash("tampered"), "issued_revision": status.Revision}).Error; e != nil {
		t.Fatal(e)
	}
	if _, e = s.Snapshot(machine(spec), spec.ServiceID); !errors.Is(e, domain.ErrCorrupt) {
		t.Fatal("tampered cached signature accepted", e)
	}
	// Rebuild only this isolated fixture's inventory to verify platform-only
	// installation evidence and non-legacy enrollment after the empty freeze.
	for _, query := range []struct {
		model any
		where string
		value any
	}{
		{&Member{}, "service_id IN ?", []string{"coord-contract-api", "coord-contract-worker", "coord-project-api", "coord-settlement-api"}},
		{&Application{}, "application IN ?", []string{"contract_management", "project_management", "settlement"}},
		{&Inventory{}, "project = ?", "coord-test"},
	} {
		if e = db.Where(query.where, query.value).Delete(query.model).Error; e != nil {
			t.Fatal(e)
		}
	}
	now = time.Unix(1900000000, 0)
	empty := evidence.Report{Scope: "INSTALLATION", BoundarySupported: true, Project: "coord-test", CollectedAt: now, Complete: true}
	if e = s.FreezeInventory(ctx, empty, nil); e != nil {
		t.Fatal("proven empty installation rejected", e)
	}
	if e = s.Register(ctx, spec); e != nil {
		t.Fatal(e)
	}
	if afterEmpty, err := s.Status(ctx, spec.Application); err != nil || afterEmpty.MigrationEligible {
		t.Fatal("post-empty installation acquired legacy eligibility", err)
	}
}
