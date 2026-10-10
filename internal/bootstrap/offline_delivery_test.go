package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	license "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	coordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
)

func TestOfflineDeploymentOrderResolvesOnlyPurchasedPortalDependency(t *testing.T) {
	for _, codes := range [][]string{
		{"customer_portal", "contract_management", "customer_and_opportunity", "project_management"},
		{"customer_and_opportunity", "customer_portal"},
		{"customer_portal"},
		{"customer_and_opportunity"},
		{},
	} {
		signed := make([]core.Application, len(codes))
		for i, code := range codes {
			signed[i].Code = code
		}
		original := append([]core.Application(nil), signed...)
		ordered := offlineDeploymentOrder(signed)
		if len(ordered) != len(signed) || !reflect.DeepEqual(signed, original) && len(signed) != 0 {
			t.Fatal("changed purchased set or signed payload", codes)
		}
		positions := map[string]int{}
		for i, app := range ordered {
			positions[app.Code] = i
		}
		crm, hasCRM := positions["customer_and_opportunity"]
		portal, hasPortal := positions["customer_portal"]
		if hasCRM && hasPortal && crm >= portal {
			t.Fatal("Portal deployed before purchased CRM", codes)
		}
		for _, app := range signed {
			if _, found := positions[app.Code]; !found {
				t.Fatal("removed a purchased application", codes)
			}
		}
	}
}

type offlinePreparationSpy struct {
	state                           domain.State
	baseline                        coordination.InstallationBaseline
	report                          evidence.Report
	collects, initializes, prepares int
	err                             error
}

func (s *offlinePreparationSpy) Read(context.Context) (domain.State, error) {
	if s.state.InstanceID == "" {
		return domain.State{}, domain.ErrNotInitialized
	}
	return s.state, nil
}
func (s *offlinePreparationSpy) Initialize(_ context.Context, in license.InitializeInput, _ domain.Actor) (domain.State, error) {
	s.initializes++
	s.state = domain.State{CustomerID: in.CustomerID, Environment: in.Environment, InstanceID: "persisted-installation-id"}
	return s.state, nil
}
func (s *offlinePreparationSpy) CollectMigrationInstallationEvidence(context.Context) (evidence.Report, error) {
	s.collects++
	return s.report, s.err
}
func (s *offlinePreparationSpy) ReadInstallationBaseline(context.Context) (coordination.InstallationBaseline, error) {
	if s.baseline.InstanceID == "" {
		return s.baseline, coordination.ErrNotReady
	}
	return s.baseline, nil
}
func (s *offlinePreparationSpy) PrepareInstallation(_ context.Context, scenario string, report evidence.Report, actor domain.Actor) (coordination.InstallationBaseline, error) {
	s.prepares++
	if s.baseline.InstanceID != "" {
		if report.Project != s.baseline.Project || (scenario != s.baseline.Scenario && scenario != "expand") {
			return s.baseline, coordination.ErrConflict
		}
		return s.baseline, nil
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return s.baseline, err
	}
	s.baseline = coordination.InstallationBaseline{Scenario: scenario, Project: report.Project, InstanceID: s.state.InstanceID, Environment: s.state.Environment, EvidenceJSON: string(raw), TenantID: actor.TenantID, UserID: actor.UserID}
	return s.baseline, nil
}

func TestOfflinePrepareFourScenariosReuseIdentityAndFrozenFacts(t *testing.T) {
	for _, scenario := range []string{"fresh", "migrate", "platform-only", "expand"} {
		t.Run(scenario, func(t *testing.T) {
			r, _, l, _, o := offlineFixture(t)
			s := &offlinePreparationSpy{report: evidence.Report{Project: "owned-installation", Complete: true}}
			if err := r.ConfigureInstallation(s, s, s); err != nil {
				t.Fatal(err)
			}
			first, err := r.Prepare(context.Background(), scenario, "customer", "")
			if err != nil {
				t.Fatal(err)
			}
			s.report.Facts = []evidence.Fact{{Application: "project_management"}}
			repeat, err := r.Prepare(context.Background(), scenario, "customer", "")
			if err != nil {
				t.Fatal(err)
			}
			if repeat.InstanceID != first.InstanceID || repeat.EvidenceJSON != first.EvidenceJSON || s.collects != 1 || s.initializes != 1 || l.calls != 0 || len(o.calls) != 0 {
				t.Fatal("retry changed identity/facts or deployed without license")
			}
			if _, err = r.Prepare(context.Background(), "expand", "customer", ""); err != nil {
				t.Fatal(err)
			}
			if s.collects != 1 {
				t.Fatal("expansion recollected legacy facts")
			}
			if _, err = r.Prepare(context.Background(), scenario, "other-customer", ""); err == nil {
				t.Fatal("foreign customer accepted")
			}
		})
	}
}

func TestOfflinePrepareSignedIdentityAndCustomerGuard(t *testing.T) {
	r, _, l, _, _ := offlineFixture(t)
	s := &offlinePreparationSpy{report: evidence.Report{Project: "owned-installation", Complete: true}}
	if err := r.ConfigureInstallation(s, s, s); err != nil {
		t.Fatal(err)
	}
	l.result.State = domain.State{CustomerID: "customer", Environment: "production", InstanceID: "signed-instance"}
	raw := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"customer_id":"customer"}`)) + ".signature"
	if _, err := r.Prepare(context.Background(), "fresh", "foreign", raw); err == nil || l.calls != 0 {
		t.Fatal("customer mismatch mutated identity")
	}
	// Import spy models the verified service's persisted identity result; cryptographic
	// verification/atomic persistence is covered by the real delivery service tests.
	l.onImport = func() { s.state = l.result.State }
	if _, err := r.Prepare(context.Background(), "fresh", "customer", raw); err != nil {
		t.Fatal(err)
	}
	if s.initializes != 0 || l.calls != 1 || s.baseline.InstanceID != "signed-instance" {
		t.Fatal("signed identity replaced")
	}
}

func TestOfflinePrepareFailsClosedOnAuthorityEvidenceAndBinding(t *testing.T) {
	for _, mode := range []string{"authority", "evidence", "environment", "baseline-identity", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			r, i, l, _, o := offlineFixture(t)
			s := &offlinePreparationSpy{state: domain.State{CustomerID: "customer", Environment: "production", InstanceID: "persisted"}, report: evidence.Report{Project: "owned", Complete: true}}
			scenario := "fresh"
			switch mode {
			case "authority":
				i.err = errors.New("administrator revoked")
			case "evidence":
				s.err = errors.New("agent unavailable")
			case "environment":
				s.state.Environment = "test"
			case "baseline-identity":
				s.baseline = coordination.InstallationBaseline{InstanceID: "foreign", Environment: "production", Project: "owned"}
			case "scenario":
				scenario = "unknown"
			}
			if err := r.ConfigureInstallation(s, s, s); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Prepare(context.Background(), scenario, "customer", ""); err == nil {
				t.Fatal("invalid preparation succeeded")
			}
			if s.initializes != 0 || s.prepares != 0 || l.calls != 0 || len(o.calls) != 0 {
				t.Fatal("failed preparation changed persisted installation or deployed")
			}
		})
	}
}

func TestOfflineMigrateOnlyFrozenSetAndNeverActivates(t *testing.T) {
	for _, enforced := range []bool{false, true} {
		r, _, l, c, o := offlineFixture(t)
		s := &offlinePreparationSpy{state: domain.State{InstanceID: "existing", Environment: "production", CustomerID: "customer"}, baseline: coordination.InstallationBaseline{InstanceID: "existing", Environment: "production", Project: "owned", EvidenceJSON: `{"project":"owned","complete":true,"facts":[{"application":"project_management","environment":"prod"}]}`}}
		if err := r.ConfigureInstallation(s, s, s); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		c.status.MigrationEligible = true
		c.status.Members = []coordination.Member{{ReadyAt: &now, AckAt: &now, IssuedRevision: 2, AckRevision: 2}}
		if enforced {
			c.status.State = runtime.Enforced
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := r.MigrateExisting(ctx)
		cancel()
		if err != nil || out.Status != "MIGRATION_READY" || len(out.Applications) != 1 || out.Applications[0] != "project_management" {
			t.Fatalf("migration: %+v %v", out, err)
		}
		if c.calls != 0 || l.calls != 0 || o.invalid {
			t.Fatal("migration imported/activated/new application deployed")
		}
		if enforced && c.status.State != runtime.Enforced {
			t.Fatal("enforced app reverted")
		}
	}
}

func TestOfflineMigrationRequiresActualCurrentACKAndEligibility(t *testing.T) {
	for _, mode := range []string{"health-only", "stale-ack", "no-eligibility", "stale-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _, c, _ := offlineFixture(t)
			s := &offlinePreparationSpy{state: domain.State{InstanceID: "existing", Environment: "production"}, baseline: coordination.InstallationBaseline{InstanceID: "existing", Environment: "production", Project: "owned", EvidenceJSON: `{"project":"owned","complete":true,"facts":[{"application":"project_management","environment":"prod"}]}`}}
			if err := r.ConfigureInstallation(s, s, s); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			c.status.MigrationEligible = true
			c.status.Members = []coordination.Member{{ReadyAt: &now, AckAt: &now, IssuedRevision: 2, AckRevision: 2}}
			switch mode {
			case "health-only":
				c.status.Members[0].AckAt = nil
			case "stale-ack":
				c.status.Members[0].AckRevision = 1
			case "no-eligibility":
				c.status.MigrationEligible = false
			case "stale-snapshot":
				c.status.SnapshotCurrent = false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if _, err := r.MigrateExisting(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("incomplete transition accepted: %v", err)
			}
			if c.calls != 0 {
				t.Fatal("migration enforced without license")
			}
		})
	}
}

type offlineIdentitySpy struct {
	principal authctx.Principal
	err       error
}

func (s *offlineIdentitySpy) OfflineBootstrapPrincipal(context.Context, time.Time) (authctx.Principal, error) {
	return s.principal, s.err
}

type offlineLicenseSpy struct {
	calls       int
	result      license.DeliveryImportResult
	actor       domain.Actor
	environment string
	onImport    func()
}

func (s *offlineLicenseSpy) ImportDelivery(_ context.Context, _ string, env string, a domain.Actor) (license.DeliveryImportResult, error) {
	s.calls++
	if s.onImport != nil {
		s.onImport()
	}
	s.actor = a
	s.environment = env
	return s.result, nil
}

type offlineCoordinatorSpy struct {
	status  coordination.Status
	calls   int
	confirm bool
}

func (s *offlineCoordinatorSpy) Status(context.Context, string) (coordination.Status, error) {
	return s.status, nil
}
func (s *offlineCoordinatorSpy) BeginActivation(context.Context, string, uint64, domain.Actor) error {
	s.calls++
	s.status.State = runtime.Enforced
	if s.confirm {
		now := time.Now()
		s.status.Members = []coordination.Member{{ServiceID: "project-api", ReadyAt: &now, AckAt: &now, IssuedRevision: s.status.Revision, AckRevision: s.status.Revision}}
	}
	return nil
}

type offlineOnboardingSpy struct {
	state   string
	calls   []string
	invalid bool
}

func (s *offlineOnboardingSpy) write(w http.ResponseWriter, r *http.Request, state string) {
	if r.URL.Host != "offline-maintenance.invalid" {
		s.invalid = true
	}
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok || p.User.ID != "bootstrap-user" {
		s.invalid = true
	}
	s.calls = append(s.calls, r.URL.Path)
	if r.Method == http.MethodPost {
		var payload map[string]string
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["application_code"] != "project_management" || payload["environment"] != "prod" || payload["upstream_url"] != "http://project-api:8082" {
			s.invalid = true
		}
		if r.URL.Path != "/api/v1/subsystem-directory" && payload["application_name"] != "" {
			s.invalid = true
		}
	}
	if state != "" {
		s.state = state
	}
	if s.state == "" && r.Method == http.MethodGet {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"status": s.state}})
}
func (s *offlineOnboardingSpy) RegisterSubsystemDirectory(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, registry.SubsystemDeploymentStatusUnmanaged)
}
func (s *offlineOnboardingSpy) AdoptSubsystem(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, registry.SubsystemDeploymentStatusReady)
}
func (s *offlineOnboardingSpy) UpdateSubsystem(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, registry.SubsystemDeploymentStatusReady)
}
func (s *offlineOnboardingSpy) GetSubsystemStatus(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, "")
}

func offlineFixture(t *testing.T) (*OfflineDeliveryRunner, *offlineIdentitySpy, *offlineLicenseSpy, *offlineCoordinatorSpy, *offlineOnboardingSpy) {
	t.Helper()
	p := authctx.Principal{Tenant: authctx.ReferenceName{ID: "bootstrap-tenant", Code: "default"}, User: authctx.ReferenceName{ID: "bootstrap-user"}, Account: authctx.ReferenceName{ID: "bootstrap-account"}, Roles: []authctx.ReferenceName{{Code: "platform-super-admin"}}, PermissionCodes: append([]string(nil), offlinePermissions...)}
	i := &offlineIdentitySpy{principal: p}
	l := &offlineLicenseSpy{result: license.DeliveryImportResult{Status: "IMPORTED", State: domain.State{Current: &core.License{Applications: []core.Application{{Code: "project_management"}}}}}}
	c := &offlineCoordinatorSpy{confirm: true, status: coordination.Status{SnapshotCurrent: true, Application: coordination.Application{Application: "project_management", State: runtime.Pending, Revision: 2}}}
	o := &offlineOnboardingSpy{}
	cfg := config.Config{Environment: "production"}
	cfg.Audit.EnvironmentCode = "prod"
	cfg.HTTP.PublicBaseURL = "https://platform.example.test"
	r, err := NewOfflineDeliveryRunner(i, l, c, o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.interval = time.Millisecond
	return r, i, l, c, o
}

func TestOfflineDeliveryRequiresPersistedPermissions(t *testing.T) {
	for _, mode := range []string{"role", "permission", "tenant", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			r, i, l, _, o := offlineFixture(t)
			switch mode {
			case "role":
				i.principal.Roles = nil
			case "permission":
				i.principal.PermissionCodes = nil
			case "tenant":
				i.principal.Tenant.Code = "another"
			case "disabled":
				i.err = errors.New("disabled")
			}
			if _, err := r.Import(context.Background(), "signed"); err == nil {
				t.Fatal("unauthorized administrator accepted")
			}
			if l.calls != 0 || len(o.calls) != 0 {
				t.Fatal("unauthorized operation performed")
			}
		})
	}
}
func TestOfflineDeliveryControlledActivationAndResume(t *testing.T) {
	r, _, l, c, o := offlineFixture(t)
	out, err := r.Activate(context.Background(), "signed")
	if err != nil || out.Status != "ENFORCED" || len(out.Applications) != 1 || c.calls != 1 || o.invalid {
		t.Fatal("activation failed", out, err, o.calls)
	}
	if l.environment != "production" || l.actor.UserID != "bootstrap-user" {
		t.Fatal("incorrect signature environment or actor")
	}
	initial := len(o.calls)
	out, err = r.Activate(context.Background(), "signed")
	if err != nil || out.Status != "ENFORCED" || c.calls != 1 {
		t.Fatal("resume not idempotent", err)
	}
	for _, path := range o.calls[initial:] {
		if path != "/api/v1/subsystem-status" {
			t.Fatal("completed application reprovisioned")
		}
	}
}
func TestOfflineDeliveryNeverSubstitutesHealthForACK(t *testing.T) {
	r, _, _, c, o := offlineFixture(t)
	c.confirm = false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	out, err := r.Activate(ctx, "signed")
	if !errors.Is(err, context.DeadlineExceeded) || out.Status == "ENFORCED" || len(out.Applications) != 0 || o.state != registry.SubsystemDeploymentStatusReady {
		t.Fatal("healthy but unconfirmed deployment accepted", out, err)
	}
}
func TestOfflineDeliveryFutureGrantDoesNotDeploy(t *testing.T) {
	r, _, l, c, o := offlineFixture(t)
	l.result.Status = "PENDING_EFFECTIVE_DATE"
	if out, err := r.Activate(context.Background(), "signed"); err == nil || out.Status != "PENDING_EFFECTIVE_DATE" {
		t.Fatal("future grant activated")
	}
	if len(o.calls) != 0 || c.calls != 0 {
		t.Fatal("future grant mutated runtime")
	}
}
func TestOfflineDeliveryControlledRetry(t *testing.T) {
	r, _, _, _, o := offlineFixture(t)
	o.state = registry.SubsystemDeploymentStatusFailed
	if _, err := r.Activate(context.Background(), "signed"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(o.calls, ","), "/api/v1/subsystem-retry") || o.invalid {
		t.Fatal("failed deployment was not safely retried", o.calls)
	}
}

func TestEnforcementCompleteRejectsStaleOrMissingMembers(t *testing.T) {
	now := time.Now()
	s := coordination.Status{SnapshotCurrent: true, Application: coordination.Application{State: runtime.Enforced, Revision: 4}, Members: []coordination.Member{{ReadyAt: &now, AckAt: &now, AckRevision: 4, IssuedRevision: 4}}}
	if !enforcementComplete(s) {
		t.Fatal("current confirmations rejected")
	}
	s.SnapshotCurrent = false
	if enforcementComplete(s) {
		t.Fatal("old renewal snapshot and old acknowledgements accepted")
	}
	s.SnapshotCurrent = true
	s.Members[0].AckRevision = 3
	if enforcementComplete(s) {
		t.Fatal("old confirmation accepted")
	}
	s.Members = nil
	if enforcementComplete(s) {
		t.Fatal("empty confirmations accepted")
	}
}
