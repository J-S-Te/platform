package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	license "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
)

type enforcementReaderSpy struct {
	state       domain.State
	err         error
	reads       int
	initializes int
	onRead      func(int)
}

func (s *enforcementReaderSpy) Read(context.Context) (domain.State, error) {
	s.reads++
	if s.onRead != nil {
		s.onRead(s.reads)
	}
	state := s.state
	if state.Current != nil {
		value := *state.Current
		value.Applications = append([]core.Application(nil), value.Applications...)
		state.Current = &value
	}
	if state.Pending != nil {
		value := *state.Pending
		value.Applications = append([]core.Application(nil), value.Applications...)
		state.Pending = &value
	}
	return state, s.err
}

func (s *enforcementReaderSpy) Initialize(context.Context, license.InitializeInput, domain.Actor) (domain.State, error) {
	s.initializes++
	return domain.State{}, errors.New("verification must never initialize")
}

type enforcementApprovalSpy struct {
	*offlinePreparationSpy
	releases map[string]registry.RuntimeLicenseReleaseApproval
	reads    map[string]int
	err      error
	onRead   func(string, int)
}

func (s *enforcementApprovalSpy) ApprovedRuntimeLicenseLifecycle(_ context.Context, app, env string) (registry.RuntimeLicenseReleaseApproval, error) {
	s.reads[app]++
	if s.onRead != nil {
		s.onRead(app, s.reads[app])
	}
	if env != "prod" {
		return registry.RuntimeLicenseReleaseApproval{}, errors.New("wrong application environment")
	}
	value := s.releases[app]
	value.Components = append([]registry.RuntimeLicenseApproval(nil), value.Components...)
	value.RequiredServiceIDs = append([]string(nil), value.RequiredServiceIDs...)
	return value, s.err
}

type enforcementCoordinatorSpy struct {
	statuses map[string]coordination.Status
	reads    map[string]int
	err      error
	starts   int
	onStatus func(string, int)
}

func (s *enforcementCoordinatorSpy) Status(_ context.Context, app string) (coordination.Status, error) {
	s.reads[app]++
	if s.onStatus != nil {
		s.onStatus(app, s.reads[app])
	}
	value := s.statuses[app]
	value.Members = append([]coordination.Member(nil), value.Members...)
	return value, s.err
}

func (s *enforcementCoordinatorSpy) BeginActivation(context.Context, string, uint64, domain.Actor) error {
	s.starts++
	return errors.New("verification must never start activation")
}

type enforcementFixture struct {
	r                 *OfflineDeliveryRunner
	i                 *offlineIdentitySpy
	imports           *offlineLicenseSpy
	onboarding        *offlineOnboardingSpy
	reader            *enforcementReaderSpy
	approvals         *enforcementApprovalSpy
	coordinator       *enforcementCoordinatorSpy
	now               time.Time
	verify            offlineSnapshotVerification
	vendor, platform  ed25519.PrivateKey
	currentRaw        string
	snapshots         map[string]runtime.PlatformSnapshot
	platformPublicKey ed25519.PublicKey
}

func newEnforcementFixture(t *testing.T, portalConditional bool) *enforcementFixture {
	t.Helper()
	r, i, imports, _, onboarding := offlineFixture(t)
	f := &enforcementFixture{r: r, i: i, imports: imports, onboarding: onboarding, now: time.Now().UTC().Truncate(time.Second), snapshots: map[string]runtime.PlatformSnapshot{}}
	_, vendor, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	platformPublic, platform, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.vendor, f.platform, f.platformPublicKey = vendor, platform, platformPublic
	f.verify = func(raw string, binding runtime.Binding) (runtime.PlatformSnapshot, error) {
		return runtime.VerifySnapshot(raw, binding, map[string]ed25519.PublicKey{"test-platform": platformPublic}, map[string]ed25519.PublicKey{"test-vendor": vendor.Public().(ed25519.PublicKey)})
	}
	l := core.License{ProtocolVersion: core.ProtocolVersion, Issuer: core.Issuer, ID: "test-only-license", Version: 7, CustomerID: "test-customer", ProductID: core.Product, Environment: "production", InstanceID: "test-native-instance", IssuedAt: f.now.Add(-2 * time.Hour).Unix(), NotBefore: f.now.Add(-time.Hour).Unix()}
	services := map[string][]string{
		"customer_and_opportunity": {"customer-api", "customer-opportunity-alert-worker", "customer-owner-notification-worker", "customer-contract-transfer-worker", "customer-presale-alert-worker", "customer-presale-assignment-notification-worker", "customer-presale-progress-notification-worker", "customer-notification-delivery-worker", "customer-presale-worker"},
		"customer_portal":          {"portal-api", "portal-report-worker"},
		"contract_management":      {"contract-api", "contract-worker"},
		"project_management":       {"project-api", "project-sla-notifier"},
		"settlement":               {"settlement-api", "settlement-worker"},
		"data_analysis":            {"data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"},
	}
	if portalConditional {
		services["customer_portal"] = append(services["customer_portal"], "portal-invite-compensation-worker")
	}
	f.approvals = &enforcementApprovalSpy{offlinePreparationSpy: &offlinePreparationSpy{}, releases: map[string]registry.RuntimeLicenseReleaseApproval{}, reads: map[string]int{}}
	f.coordinator = &enforcementCoordinatorSpy{statuses: map[string]coordination.Status{}, reads: map[string]int{}}
	for _, app := range []string{"customer_and_opportunity", "customer_portal", "contract_management", "project_management", "settlement", "data_analysis"} {
		l.Applications = append(l.Applications, core.Application{Code: app, Kind: "FULL", NotBefore: l.NotBefore, ExpiresAt: f.now.Add(time.Hour).Unix()})
		release := registry.RuntimeLicenseReleaseApproval{ReleaseGeneration: "reviewed-v2"}
		for _, id := range services[app] {
			release.Components = append(release.Components, registry.RuntimeLicenseApproval{ServiceID: id, Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)})
			release.RequiredServiceIDs = append(release.RequiredServiceIDs, id)
		}
		f.approvals.releases[app] = release
	}
	raw, err := core.Sign(l, "test-vendor", vendor)
	if err != nil {
		t.Fatal(err)
	}
	f.currentRaw = raw
	f.reader = &enforcementReaderSpy{state: domain.State{InstanceID: l.InstanceID, CustomerID: l.CustomerID, Environment: l.Environment, Revision: 42, CurrentVersion: l.Version, HighestVersion: l.Version, CurrentDigest: enforcementHash(raw), Current: &l, HighestObservedAt: f.now.Unix()}}
	if err = r.ConfigureInstallation(f.approvals, f.approvals, f.reader); err != nil {
		t.Fatal(err)
	}
	r.coordinator = f.coordinator
	verifier, err := registry.NewRuntimeLicenseLifecycleVerifier(f.approvals)
	if err != nil {
		t.Fatal(err)
	}
	for app, release := range f.approvals.releases {
		digest, err := registry.RuntimeLicenseReleaseDigest(release)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := verifier.VerifyRuntimeLifecycle(context.Background(), coordination.LifecycleInput{Application: app, Environment: "prod", ReleaseDigest: digest})
		if err != nil {
			t.Fatal(err)
		}
		status := coordination.Status{Application: coordination.Application{Application: app, State: runtime.Enforced, Revision: 31}, DeploymentRevision: f.reader.state.Revision, SnapshotCurrent: true}
		for _, spec := range binding.Specs {
			snapshot := runtime.PlatformSnapshot{Protocol: 1, InstanceID: l.InstanceID, Environment: l.Environment, Application: app, ServiceID: spec.ServiceID, Revision: status.Revision, EnforcementState: runtime.Enforced, CurrentLicenseJWS: raw, HighestLicenseVersion: l.Version, SnapshotIssuedAt: f.now.Add(-time.Minute).Unix()}
			f.snapshots[spec.ServiceID] = snapshot
			issued, e := runtime.SignSnapshot(snapshot, "test-platform", platform)
			if e != nil {
				t.Fatal(e)
			}
			heartbeat := f.now.Add(-5 * time.Second)
			status.Members = append(status.Members, coordination.Member{Application: app, Environment: "prod", ServiceID: spec.ServiceID, OAuthClientID: spec.OAuthClientID, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest, ReadyAt: &heartbeat, AckAt: &heartbeat, IssuedRevision: status.Revision, AckRevision: status.Revision, RawJWS: issued, IssuedDigest: enforcementHash(issued)})
		}
		f.coordinator.statuses[app] = status
	}
	f.approvals.reads = map[string]int{}
	return f
}

func enforcementHash(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func (f *enforcementFixture) run() (OfflineEnforcementVerification, error) {
	return f.r.verifyEnforcement(context.Background(), f.verify, func() time.Time { return f.now })
}

func (f *enforcementFixture) assertNoManagementWrites(t *testing.T) {
	t.Helper()
	if f.reader.initializes != 0 || f.imports.calls != 0 || f.coordinator.starts != 0 || len(f.onboarding.calls) != 0 || f.approvals.collects != 0 || f.approvals.prepares != 0 {
		t.Fatal("verification imported, prepared, initialized, deployed or activated")
	}
}

func (f *enforcementFixture) changeMember(app string, change func(*coordination.Member)) {
	status := f.coordinator.statuses[app]
	change(&status.Members[0])
	f.coordinator.statuses[app] = status
}

func (f *enforcementFixture) changeSnapshot(t *testing.T, app string, change func(*runtime.PlatformSnapshot)) {
	t.Helper()
	f.changeMember(app, func(member *coordination.Member) {
		snapshot := f.snapshots[member.ServiceID]
		change(&snapshot)
		raw, err := runtime.SignSnapshot(snapshot, "test-platform", f.platform)
		if err != nil {
			t.Fatal(err)
		}
		member.RawJWS, member.IssuedDigest = raw, enforcementHash(raw)
	})
}

func TestOfflineVerifyEnforcementFullPurchaseAndSelectedConditionalMembers(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		t.Run(map[bool]string{false: "portal-condition-absent", true: "portal-condition-present"}[conditional], func(t *testing.T) {
			f := newEnforcementFixture(t, conditional)
			out, err := f.run()
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "ENFORCED" || out.LicenseVersion != 7 || out.InstanceID != "test-native-instance" || len(out.Applications) != 6 || len(out.Runtime) != 6 || f.reader.reads != 2 {
				t.Fatalf("incomplete purchase result: %+v", out)
			}
			for _, proof := range out.Runtime {
				if f.coordinator.reads[proof.Application] != 2 || f.approvals.reads[proof.Application] != 3 {
					t.Fatal("full set was not re-observed")
				}
				if proof.Application == "customer_and_opportunity" && len(proof.Components) != 9 {
					t.Fatal("CRM transfer consumer missing")
				}
				if proof.Application == "customer_portal" && len(proof.Components) != map[bool]int{false: 2, true: 3}[conditional] {
					t.Fatal("conditional selection was widened or omitted")
				}
			}
			raw, err := json.Marshal(out)
			if err != nil || strings.Contains(string(raw), f.currentRaw) || strings.Contains(string(raw), "client_secret") || strings.Contains(string(raw), "raw_jws") {
				t.Fatal("verification disclosed authority material")
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementFailsClosedOnLicenseAuthority(t *testing.T) {
	cases := map[string]func(*enforcementFixture){
		"administrator":          func(f *enforcementFixture) { f.i.err = errors.New("revoked") },
		"no-reader":              func(f *enforcementFixture) { f.r.initializer = nil },
		"no-approval-reader":     func(f *enforcementFixture) { f.r.source = &offlinePreparationSpy{} },
		"uninitialized":          func(f *enforcementFixture) { f.reader.err = domain.ErrNotInitialized },
		"trust-not-configured":   func(f *enforcementFixture) { f.reader.err = domain.ErrTrustNotConfigured },
		"corrupt-signature":      func(f *enforcementFixture) { f.reader.err = domain.ErrCorrupt },
		"rollback-clock":         func(f *enforcementFixture) { f.reader.err = domain.ErrClock },
		"clock-blocked":          func(f *enforcementFixture) { f.reader.state.ClockBlocked = true },
		"no-current":             func(f *enforcementFixture) { f.reader.state.Current = nil },
		"wrong-instance":         func(f *enforcementFixture) { f.reader.state.InstanceID = "other" },
		"wrong-customer":         func(f *enforcementFixture) { f.reader.state.CustomerID = "other" },
		"wrong-environment":      func(f *enforcementFixture) { f.reader.state.Environment = "test" },
		"wrong-version":          func(f *enforcementFixture) { f.reader.state.CurrentVersion++ },
		"invalid-current-digest": func(f *enforcementFixture) { f.reader.state.CurrentDigest = "untrusted" },
		"zero-revision":          func(f *enforcementFixture) { f.reader.state.Revision = 0 },
		"unobserved-clock":       func(f *enforcementFixture) { f.reader.state.HighestObservedAt = 0 },
		"future-clock-anchor":    func(f *enforcementFixture) { f.reader.state.HighestObservedAt = f.now.Add(301 * time.Second).Unix() },
		"empty-purchase":         func(f *enforcementFixture) { f.reader.state.Current.Applications = nil },
		"license-not-effective":  func(f *enforcementFixture) { f.reader.state.Current.NotBefore = f.now.Add(time.Hour).Unix() },
		"one-expired-purchase":   func(f *enforcementFixture) { f.reader.state.Current.Applications[5].ExpiresAt = f.now.Unix() },
		"one-future-purchase": func(f *enforcementFixture) {
			f.reader.state.Current.Applications[5].NotBefore = f.now.Add(time.Minute).Unix()
		},
		"pending-without-token": func(f *enforcementFixture) { f.reader.state.PendingDigest = strings.Repeat("a", 64) },
		"unaccounted-version":   func(f *enforcementFixture) { f.reader.state.HighestVersion++ },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newEnforcementFixture(t, false)
			mutate(f)
			out, err := f.run()
			if !errors.Is(err, ErrOfflineEnforcementIncomplete) || out.Status == "ENFORCED" {
				t.Fatalf("invalid authority accepted: %+v %v", out, err)
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementRequiresExactCurrentNaturalACK(t *testing.T) {
	app := "customer_and_opportunity"
	cases := map[string]func(*enforcementFixture){
		"status-error":   func(f *enforcementFixture) { f.coordinator.err = errors.New("unavailable") },
		"approval-error": func(f *enforcementFixture) { f.approvals.err = errors.New("unavailable") },
		"missing-transfer-worker": func(f *enforcementFixture) {
			s := f.coordinator.statuses[app]
			for index, member := range s.Members {
				if member.ServiceID == "customer-contract-transfer-worker" {
					s.Members = append(s.Members[:index], s.Members[index+1:]...)
					break
				}
			}
			f.coordinator.statuses[app] = s
		},
		"duplicate-member": func(f *enforcementFixture) {
			s := f.coordinator.statuses[app]
			s.Members[1] = s.Members[0]
			f.coordinator.statuses[app] = s
		},
		"unexpected-member": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.ServiceID = "not-approved" })
		},
		"wrong-oauth-generation": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.OAuthClientID = "old-client" })
		},
		"wrong-image": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.ImageDigest = "sha256:" + strings.Repeat("c", 64) })
		},
		"wrong-coverage": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.CoverageDigest = "sha256:" + strings.Repeat("c", 64) })
		},
		"wrong-member-app": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.Application = "customer_portal" })
		},
		"wrong-member-env": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.Environment = "production" })
		},
		"retired-member": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.RetiredAt = &f.now })
		},
		"missing-ready":       func(f *enforcementFixture) { f.changeMember(app, func(m *coordination.Member) { m.ReadyAt = nil }) },
		"missing-ack":         func(f *enforcementFixture) { f.changeMember(app, func(m *coordination.Member) { m.AckAt = nil }) },
		"old-ack-revision":    func(f *enforcementFixture) { f.changeMember(app, func(m *coordination.Member) { m.AckRevision-- }) },
		"old-issued-revision": func(f *enforcementFixture) { f.changeMember(app, func(m *coordination.Member) { m.IssuedRevision-- }) },
		"stale-ready": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { at := f.now.Add(-61 * time.Second); m.ReadyAt = &at })
		},
		"stale-ack": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { at := f.now.Add(-61 * time.Second); m.AckAt = &at })
		},
		"future-ack": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { at := f.now.Add(2 * time.Second); m.AckAt = &at })
		},
		"wrong-issued-digest": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.IssuedDigest = strings.Repeat("0", 64) })
		},
		"missing-snapshot": func(f *enforcementFixture) { f.changeMember(app, func(m *coordination.Member) { m.RawJWS = "" }) },
		"runtime-signature-tampered": func(f *enforcementFixture) {
			f.changeMember(app, func(m *coordination.Member) { m.RawJWS += "A"; m.IssuedDigest = enforcementHash(m.RawJWS) })
		},
		"old-vendor-license-content": func(f *enforcementFixture) { f.reader.state.CurrentDigest = strings.Repeat("c", 64) },
		"wrong-signed-binding": func(f *enforcementFixture) {
			f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.InstanceID = "other-native-instance" })
		},
		"wrong-signed-revision": func(f *enforcementFixture) {
			f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.Revision-- })
		},
		"wrong-signed-version": func(f *enforcementFixture) {
			f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.HighestLicenseVersion++ })
		},
		"wrong-signed-state": func(f *enforcementFixture) {
			f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.EnforcementState = runtime.Applying })
		},
		"future-snapshot": func(f *enforcementFixture) {
			f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.SnapshotIssuedAt = f.now.Add(time.Second).Unix() })
		},
	}
	for _, state := range []string{"pending", "applying", "stale-snapshot", "wrong-status-app", "old-deployment", "migration-eligible"} {
		cases[state] = func(f *enforcementFixture) {
			s := f.coordinator.statuses[app]
			switch state {
			case "pending":
				s.State = runtime.Pending
			case "applying":
				s.State = runtime.Applying
			case "stale-snapshot":
				s.SnapshotCurrent = false
			case "wrong-status-app":
				s.Application.Application = "customer_portal"
			case "old-deployment":
				s.DeploymentRevision--
			case "migration-eligible":
				s.MigrationEligible = true
			}
			f.coordinator.statuses[app] = s
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newEnforcementFixture(t, true)
			mutate(f)
			out, err := f.run()
			if !errors.Is(err, ErrOfflineEnforcementIncomplete) || out.Status == "ENFORCED" {
				t.Fatalf("missing real confirmation accepted: %+v %v", out, err)
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementDetectsConcurrentChangesAndCancellation(t *testing.T) {
	for _, change := range []string{"release", "earlier-app-ack", "application-revision", "license", "administrator", "cancellation"} {
		t.Run(change, func(t *testing.T) {
			f := newEnforcementFixture(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "release":
				f.approvals.onRead = func(app string, count int) {
					if count == 3 {
						release := f.approvals.releases[app]
						release.ReleaseGeneration = "changed-v3"
						f.approvals.releases[app] = release
					}
				}
			case "earlier-app-ack", "application-revision":
				f.coordinator.onStatus = func(app string, count int) {
					if count == 2 {
						if change == "earlier-app-ack" {
							f.changeMember(app, func(m *coordination.Member) { m.AckRevision-- })
						} else {
							status := f.coordinator.statuses[app]
							status.Revision++
							f.coordinator.statuses[app] = status
						}
					}
				}
			case "license":
				f.reader.onRead = func(count int) {
					if count == 2 {
						f.reader.state.Current.Version++
						f.reader.state.CurrentVersion++
						f.reader.state.HighestVersion++
						f.reader.state.Revision++
					}
				}
			case "administrator":
				f.reader.onRead = func(count int) {
					if count == 2 {
						f.i.err = errors.New("revoked during checks")
					}
				}
			case "cancellation":
				f.coordinator.onStatus = func(_ string, _ int) { cancel() }
			}
			out, err := f.r.verifyEnforcement(ctx, f.verify, func() time.Time { return f.now })
			if err == nil || out.Status == "ENFORCED" {
				t.Fatal("changed/cancelled observation returned success")
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementUsesPublicOnlyVerifierAndFixedErrors(t *testing.T) {
	f := newEnforcementFixture(t, false)
	der, err := x509.MarshalPKIXPublicKey(f.platformPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Environment: "production"}
	cfg.Audit.EnvironmentCode = "prod"
	cfg.Auth.JWTIssuer, cfg.Auth.ApplicationJWTAudience, cfg.Auth.JWTPublicKeyPath = "test", "test-audience", path
	cfg.Auth.JWTPrivateKeyPath = filepath.Join(t.TempDir(), "must-not-be-read-private.pem")
	publicOnly, err := security.LoadApplicationJWTVerifier(cfg.Auth.JWTIssuer, cfg.Auth.ApplicationJWTAudience, path)
	if err != nil {
		t.Fatal(err)
	}
	for app, status := range f.coordinator.statuses {
		for index, member := range status.Members {
			raw, err := runtime.SignSnapshot(f.snapshots[member.ServiceID], publicOnly.LicenseSnapshotKeyID(), f.platform)
			if err != nil {
				t.Fatal(err)
			}
			status.Members[index].RawJWS, status.Members[index].IssuedDigest = raw, enforcementHash(raw)
		}
		f.coordinator.statuses[app] = status
	}
	// 生产入口只信任编译时内置的供应商密钥集合，不信任这些
	// 临时测试密钥；真实加密校验路径必须拒绝仅供测试的令牌。
	out, err := f.r.VerifyEnforcement(context.Background(), cfg)
	if !errors.Is(err, ErrOfflineEnforcementIncomplete) || out.Status == "ENFORCED" || strings.Contains(err.Error(), "verification key unavailable") {
		t.Fatalf("public-only verifier missing or untrusted test token accepted: %+v %v", out, err)
	}
	f.approvals.err = errors.New("secret-client-credential:" + f.currentRaw)
	_, err = f.run()
	if err == nil || strings.Contains(err.Error(), f.currentRaw) || strings.Contains(err.Error(), "secret-client") {
		t.Fatal("upstream error leaked authority material")
	}
	f.assertNoManagementWrites(t)
}

func TestOfflineVerifyEnforcementRejectsInvalidReleaseBoundaries(t *testing.T) {
	for _, problem := range []string{"empty", "duplicate-component", "missing-required", "duplicate-required", "unapproved-required", "invalid-protocol", "invalid-generation", "changed-second-read"} {
		t.Run(problem, func(t *testing.T) {
			f := newEnforcementFixture(t, false)
			app := "contract_management"
			release := f.approvals.releases[app]
			switch problem {
			case "empty":
				release.Components = nil
			case "duplicate-component":
				release.Components[1] = release.Components[0]
			case "missing-required":
				release.RequiredServiceIDs = nil
			case "duplicate-required":
				release.RequiredServiceIDs[1] = release.RequiredServiceIDs[0]
			case "unapproved-required":
				release.RequiredServiceIDs[0] = "unapproved-worker"
			case "invalid-protocol":
				release.Components[0].Protocol = 2
			case "invalid-generation":
				release.ReleaseGeneration = "not a valid generation"
			case "changed-second-read":
				f.approvals.onRead = func(code string, count int) {
					if code == app && count == 2 {
						r := f.approvals.releases[app]
						r.ReleaseGeneration = "changed-v3"
						f.approvals.releases[app] = r
					}
				}
			}
			f.approvals.releases[app] = release
			if out, err := f.run(); err == nil || out.Status == "ENFORCED" {
				t.Fatal("invalid/unsteady release boundary accepted")
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementFuturePendingIsComparedButNeverActivated(t *testing.T) {
	for _, mode := range []string{"future", "due-not-observed", "pending-slot-missing", "wrong-pending-customer"} {
		t.Run(mode, func(t *testing.T) {
			f := newEnforcementFixture(t, false)
			pending := *f.reader.state.Current
			pending.ID, pending.Version, pending.NotBefore = "test-only-renewal", 8, f.now.Add(30*time.Minute).Unix()
			pending.Applications = append([]core.Application(nil), pending.Applications...)
			for index := range pending.Applications {
				pending.Applications[index].NotBefore = pending.NotBefore
				pending.Applications[index].ExpiresAt = f.now.Add(2 * time.Hour).Unix()
			}
			raw, err := core.Sign(pending, "test-vendor", f.vendor)
			if err != nil {
				t.Fatal(err)
			}
			f.reader.state.Pending, f.reader.state.PendingDigest, f.reader.state.HighestVersion = &pending, enforcementHash(raw), pending.Version
			for app, status := range f.coordinator.statuses {
				for index, member := range status.Members {
					snapshot := f.snapshots[member.ServiceID]
					snapshot.PendingLicenseJWS, snapshot.HighestLicenseVersion = raw, pending.Version
					if mode == "pending-slot-missing" {
						snapshot.PendingLicenseJWS = ""
					}
					issued, err := runtime.SignSnapshot(snapshot, "test-platform", f.platform)
					if err != nil {
						t.Fatal(err)
					}
					status.Members[index].RawJWS, status.Members[index].IssuedDigest = issued, enforcementHash(issued)
				}
				f.coordinator.statuses[app] = status
			}
			if mode == "due-not-observed" {
				f.reader.state.Pending.NotBefore = f.now.Unix()
			}
			if mode == "wrong-pending-customer" {
				f.reader.state.Pending.CustomerID = "other"
			}
			out, err := f.run()
			if mode == "future" {
				if err != nil || out.Status != "ENFORCED" || out.LicenseVersion != 7 || f.reader.state.CurrentVersion != 7 || f.reader.state.Pending.Version != 8 {
					t.Fatalf("future renewal activated/ignored or current entitlement rejected: %+v %v", out, err)
				}
			} else if err == nil || out.Status == "ENFORCED" {
				t.Fatal("inconsistent pending slot accepted")
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementRejectsUntrustedPlatformAndVendorSignatures(t *testing.T) {
	for _, target := range []string{"platform", "vendor"} {
		t.Run(target, func(t *testing.T) {
			f := newEnforcementFixture(t, false)
			_, other, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			f.changeMember("contract_management", func(m *coordination.Member) {
				snapshot := f.snapshots[m.ServiceID]
				key := f.platform
				if target == "platform" {
					key = other
				} else {
					snapshot.CurrentLicenseJWS, err = core.Sign(*f.reader.state.Current, "test-vendor", other)
					if err != nil {
						t.Fatal(err)
					}
				}
				m.RawJWS, err = runtime.SignSnapshot(snapshot, "test-platform", key)
				if err != nil {
					t.Fatal(err)
				}
				m.IssuedDigest = enforcementHash(m.RawJWS)
			})
			if out, err := f.run(); err == nil || out.Status == "ENFORCED" {
				t.Fatal("untrusted cryptographic signature accepted")
			}
			f.assertNoManagementWrites(t)
		})
	}
}

func TestOfflineVerifyEnforcementDoesNotRenewOldSnapshotIssuance(t *testing.T) {
	f := newEnforcementFixture(t, false)
	for app := range f.coordinator.statuses {
		f.changeSnapshot(t, app, func(s *runtime.PlatformSnapshot) { s.SnapshotIssuedAt = f.now.Add(-24 * time.Hour).Unix() })
	}
	before := map[string][]coordination.Member{}
	for app, status := range f.coordinator.statuses {
		before[app] = append([]coordination.Member(nil), status.Members...)
	}
	if _, err := f.run(); err != nil {
		t.Fatal("durable current snapshots with fresh natural heartbeats must remain valid", err)
	}
	for app, status := range f.coordinator.statuses {
		if !reflect.DeepEqual(before[app], status.Members) {
			t.Fatal("verification refreshed snapshot or ACK")
		}
	}
	f.assertNoManagementWrites(t)
}
