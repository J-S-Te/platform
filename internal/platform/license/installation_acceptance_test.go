package license_test

// This is a protocol integration test: real MySQL, loopback HTTP transport,
// production coordinator/handlers and production durable FileRuntime. It is NOT
// a Docker installation or an OAuth token issuance acceptance test. Trust and
// authenticated machine principals are test-only fixtures, never release keys.
import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/migrations"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	driver "github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type acceptanceSigner struct {
	private          ed25519.PrivateKey
	platform, vendor map[string]ed25519.PublicKey
}

func (s acceptanceSigner) Sign(p runtime.PlatformSnapshot) (string, error) {
	return runtime.SignSnapshot(p, "isolated-platform", s.private)
}
func (s acceptanceSigner) Verify(raw string, b runtime.Binding) (runtime.PlatformSnapshot, error) {
	return runtime.VerifySnapshot(raw, b, s.platform, s.vendor)
}

func acceptanceComponents() []coordination.ServiceSpec {
	groups := []struct {
		app      string
		services []string
	}{
		{"contract_management", []string{"contract-api", "contract-worker"}},
		{"project_management", []string{"project-api", "project-sla-notifier"}},
		{"settlement", []string{"settlement-api", "settlement-worker"}},
		{"data_analysis", []string{"data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"}},
		{"customer_and_opportunity", []string{"customer-api", "customer-opportunity-alert-worker", "customer-owner-notification-worker", "customer-presale-alert-worker", "customer-presale-assignment-notification-worker", "customer-presale-progress-notification-worker", "customer-notification-delivery-worker", "customer-presale-worker"}},
		{"customer_portal", []string{"portal-api", "portal-invite-compensation-worker"}},
	}
	var specs []coordination.ServiceSpec
	for _, g := range groups {
		for _, id := range g.services {
			specs = append(specs, coordination.ServiceSpec{Application: g.app, Environment: "prod", ServiceID: id, OAuthClientID: "isolated-" + id, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)})
		}
	}
	return specs
}

// A new production execution component must expand this explicit protocol
// denominator rather than passing unnoticed under an old fixture inventory.
func TestInstallationComponentProfileParity(t *testing.T) {
	profiles := []string{"contract_management-prod.yaml", "project_management-prod.yaml", "settlement-prod.yaml", "data-analysis-prod.yaml", "customer_and_opportunity-prod.yaml", "customer_portal-prod.yaml"}
	expected := map[string]string{}
	for _, spec := range acceptanceComponents() {
		expected[spec.ServiceID] = spec.Application
	}
	for _, name := range profiles {
		raw, err := os.ReadFile(filepath.Join("../../../deploy/production/subsystems.d", name))
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Application struct {
				Code string `yaml:"code"`
			}
			Compose struct {
				Services []string `yaml:"runtime_services"`
			}
		}
		if err = yaml.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		for _, service := range p.Compose.Services {
			// Metabase has no commercial code/credential. Access is enforced by
			// the licensed BI signing and resource proxy execution components.
			if service == "data-analysis-metabase" {
				continue
			}
			if expected[service] != p.Application.Code {
				t.Fatalf("unclassified or mismatched production component %s/%s", p.Application.Code, service)
			}
			delete(expected, service)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("protocol fixtures do not match production profiles: %v", expected)
	}
}

func TestIsolatedInstallationProtocolScenarios(t *testing.T) {
	dsn := os.Getenv("LICENSE_INSTALLATION_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated LICENSE_INSTALLATION_TEST_DSN is not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid dedicated test DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "platform_license_installation_test" {
		t.Fatal("only loopback dedicated installation test database is permitted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := sqlDB.Close(); e != nil {
			t.Error(e)
		}
	}()
	ctx := context.Background()
	if _, err = migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	// The whole database belongs to this test run; no external/business database
	// is accepted above. Delete only the licensing rows used by these scenarios.
	reset := func() {
		t.Helper()
		for _, model := range []any{&coordination.LifecycleEvent{}, &coordination.RetiredClient{}, &coordination.Member{}, &coordination.Application{}, &coordination.Inventory{}, &domain.ClockRecovery{}, &domain.Event{}, &domain.Deployment{}, &domain.Artifact{}} {
			if e := db.Where("1 = 1").Delete(model).Error; e != nil {
				t.Fatal(e)
			}
		}
	}
	defer reset()
	all := acceptanceComponents()
	for _, scenario := range []struct {
		name   string
		legacy int
		freeze bool
	}{{"new-installation", 0, false}, {"existing-complete-installation", len(all), true}, {"platform-only-installation", 0, true}, {"partial-installation-with-new-subsystems", 4, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			reset()
			now := time.Unix(1900000000, 0)
			vendorPub, vendorPrivate, e := ed25519.GenerateKey(rand.Reader)
			if e != nil {
				t.Fatal(e)
			}
			platformPub, platformPrivate, e := ed25519.GenerateKey(rand.Reader)
			if e != nil {
				t.Fatal(e)
			}
			vendor := map[string]ed25519.PublicKey{"isolated-vendor": vendorPub}
			platform := map[string]ed25519.PublicKey{"isolated-platform": platformPub}
			repo, e := infrastructure.NewRepository(db)
			if e != nil {
				t.Fatal(e)
			}
			lic, e := application.NewService(repo, vendor, func() time.Time { return now })
			if e != nil {
				t.Fatal(e)
			}
			actor := domain.Actor{TenantID: "isolated-tenant", UserID: "isolated-admin"}
			state, e := lic.Initialize(ctx, application.InitializeInput{CustomerID: "isolated-customer", Environment: "production"}, actor)
			if e != nil {
				t.Fatal(e)
			}
			coordinator, e := coordination.NewService(db, lic, vendor, acceptanceSigner{platformPrivate, platform, vendor}, func() time.Time { return now }, coordination.WithApplicationEnvironment("prod"))
			if e != nil {
				t.Fatal(e)
			}
			if scenario.freeze {
				// Synthetic facts exercise the trusted inventory transaction contract
				// only; they do not count as observed Docker evidence.
				report := evidence.Report{Scope: "INSTALLATION", BoundarySupported: true, Project: "isolated-protocol", CollectedAt: now, Complete: true, Facts: []evidence.Fact{}}
				for _, spec := range all[:scenario.legacy] {
					report.Facts = append(report.Facts, evidence.Fact{Application: spec.Application, Environment: spec.Environment, Service: spec.ServiceID, ContainerID: strings.Repeat("c", 64), ImageDigest: spec.ImageDigest, Version: "isolated-v1", Protocol: "1", Running: true, StartedAt: now.Add(-time.Hour)})
					if spec.Application == "data_analysis" && spec.ServiceID == "data-analysis-api" {
						report.InfrastructureFacts = append(report.InfrastructureFacts, evidence.Fact{Application: spec.Application, Environment: spec.Environment, Service: "data-analysis-metabase", ContainerID: strings.Repeat("d", 64), ImageDigest: "sha256:" + strings.Repeat("e", 64), Running: true, StartedAt: now.Add(-time.Hour)})
					}
				}
				if e = coordinator.FreezeInventory(ctx, report, all[:scenario.legacy]); e != nil {
					t.Fatal(e)
				}
			}
			for _, spec := range all[scenario.legacy:] {
				if e = coordinator.Register(ctx, spec); e != nil {
					t.Fatal(e)
				}
			}
			// Idempotent registration must not reset qualification or identity.
			for _, spec := range all {
				if e = coordinator.Register(ctx, spec); e != nil {
					t.Fatal(e)
				}
			}
			handler, e := licensehttp.NewRuntimeHandler(coordinator)
			if e != nil {
				t.Fatal(e)
			}
			principals := map[string]appctx.Principal{}
			for _, spec := range all {
				principals["isolated-token-"+spec.ServiceID] = appctx.Principal{OAuthClientID: spec.OAuthClientID, ClientID: spec.OAuthClientID, TenantID: actor.TenantID, ApplicationID: spec.Application, ApplicationCode: spec.Application, EnvironmentID: "prod", EnvironmentCode: "prod", Scopes: map[string]struct{}{"license.runtime": {}}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if p, ok := principals[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
					r = r.WithContext(appctx.WithPrincipal(r.Context(), p))
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/ready"):
					handler.Ready(w, r)
				case strings.HasSuffix(r.URL.Path, "/snapshot"):
					handler.Snapshot(w, r)
				case strings.HasSuffix(r.URL.Path, "/ack"):
					handler.Ack(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			request := func(spec coordination.ServiceSpec, action string, body any, want int) json.RawMessage {
				t.Helper()
				var input []byte
				if body != nil {
					input, e = json.Marshal(body)
					if e != nil {
						t.Fatal(e)
					}
				}
				method := http.MethodPost
				if action == "snapshot" {
					method = http.MethodGet
				}
				r, e := http.NewRequest(method, server.URL+"/api/v1/internal/licenses/runtime/"+spec.ServiceID+"/"+action, bytes.NewReader(input))
				if e != nil {
					t.Fatal(e)
				}
				r.Header.Set("Authorization", "Bearer isolated-token-"+spec.OAuthClientID[len("isolated-"):])
				r.Header.Set("Content-Type", "application/json")
				resp, e := client.Do(r)
				if e != nil {
					t.Fatal(e)
				}
				defer resp.Body.Close()
				raw, e := io.ReadAll(io.LimitReader(resp.Body, 300000))
				if e != nil {
					t.Fatal(e)
				}
				if resp.StatusCode != want {
					t.Fatalf("%s %s: HTTP %d, expected %d", spec.ServiceID, action, resp.StatusCode, want)
				}
				var envelope struct {
					Data json.RawMessage `json:"data"`
				}
				if e = json.Unmarshal(raw, &envelope); e != nil {
					t.Fatal(e)
				}
				return envelope.Data
			}
			paths := map[string]string{}
			local := map[string]*runtime.FileRuntime{}
			for _, spec := range all {
				path := filepath.Join(t.TempDir(), "state.json")
				paths[spec.ServiceID] = path
				rt, e := runtime.NewFileRuntime(path, runtime.Binding{InstanceID: state.InstanceID, Environment: "production", Application: spec.Application, ServiceID: spec.ServiceID}, platform, vendor)
				if e != nil {
					t.Fatal(e)
				}
				local[spec.ServiceID] = rt
				var snapshot coordination.SnapshotOutput
				if e = json.Unmarshal(request(spec, "snapshot", nil, 200), &snapshot); e != nil {
					t.Fatal(e)
				}
				if e = rt.ApplySnapshot(ctx, snapshot.RawJWS, now); e != nil {
					t.Fatal(e)
				}
				got := rt.Evaluate(ctx, core.MUTATE_BUSINESS, now)
				legacy := false
				for _, old := range all[:scenario.legacy] {
					if old.Application == spec.Application {
						legacy = true
					}
				}
				if legacy && got != nil {
					t.Fatalf("legacy %s lost qualified pending continuity: %v", spec.ServiceID, got)
				}
				if !legacy && !errors.Is(got, core.ErrDenied) {
					t.Fatalf("new %s inherited legacy privilege: %v", spec.ServiceID, got)
				}
				request(spec, "ready", coordination.ReadyInput{Protocol: 2, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest}, 409)
				request(spec, "ready", coordination.ReadyInput{Protocol: 1, CoverageDigest: spec.CoverageDigest, ImageDigest: "sha256:" + strings.Repeat("d", 64)}, 409)
				request(spec, "ready", coordination.ReadyInput{Protocol: 1, CoverageDigest: spec.CoverageDigest, ImageDigest: spec.ImageDigest}, 200)
			}
			wrong := all[0]
			wrong.OAuthClientID = all[2].OAuthClientID
			request(wrong, "ready", coordination.ReadyInput{Protocol: 1, CoverageDigest: wrong.CoverageDigest, ImageDigest: wrong.ImageDigest}, 403)
			license := core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "isolated-acceptance", Version: 1, CustomerID: state.CustomerID, ProductID: core.Product, Environment: state.Environment, InstanceID: state.InstanceID, IssuedAt: now.Unix(), NotBefore: now.Unix()}
			apps := []string{"contract_management", "project_management", "settlement", "data_analysis", "customer_and_opportunity", "customer_portal"}
			for _, app := range apps {
				license.Applications = append(license.Applications, core.Application{Code: app, NotBefore: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(), Kind: "FULL"})
			}
			raw, e := core.Sign(license, "isolated-vendor", vendorPrivate)
			if e != nil {
				t.Fatal(e)
			}
			preview, e := lic.Preview(ctx, raw)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = lic.Commit(ctx, application.CommitInput{RawJWS: raw, Digest: preview.Digest, ExpectedRevision: preview.Revision, ExpectedCurrentVersion: preview.CurrentVersion, ExpectedPendingDigest: preview.PendingDigest, ConfirmChanges: true}, actor); e != nil {
				t.Fatal(e)
			}
			for _, app := range apps {
				status, e := coordinator.Status(ctx, app)
				if e != nil {
					t.Fatal(e)
				}
				if e = coordinator.BeginActivation(ctx, app, status.Revision, actor); e != nil {
					t.Fatal(e)
				}
				var members []coordination.ServiceSpec
				for _, spec := range all {
					if spec.Application == app {
						members = append(members, spec)
					}
				}
				for i, spec := range members {
					var snapshot coordination.SnapshotOutput
					if e = json.Unmarshal(request(spec, "snapshot", nil, 200), &snapshot); e != nil {
						t.Fatal(e)
					}
					if e = local[spec.ServiceID].ApplySnapshot(ctx, snapshot.RawJWS, now); e != nil {
						t.Fatal(e)
					}
					request(spec, "ack", coordination.AckInput{Revision: snapshot.Revision, Digest: strings.Repeat("d", 64)}, 409)
					request(spec, "ack", coordination.AckInput{Revision: snapshot.Revision, Digest: snapshot.Digest}, 200)
					if i < len(members)-1 {
						status, e = coordinator.Status(ctx, app)
						if e != nil || status.State != runtime.Applying {
							t.Fatalf("%s enforced before all members acknowledged: %v", app, e)
						}
					}
				}
				status, e = coordinator.Status(ctx, app)
				if e != nil || status.State != runtime.Enforced || status.MigrationEligible {
					t.Fatalf("%s activation state %v %v", app, status.State, e)
				}
			}
			// Apply ENFORCED revision before disconnect. No server contact can
			// extend the vendor's absolute expiry after this point.
			for _, spec := range all {
				var snapshot coordination.SnapshotOutput
				if e = json.Unmarshal(request(spec, "snapshot", nil, 200), &snapshot); e != nil {
					t.Fatal(e)
				}
				if e = local[spec.ServiceID].ApplySnapshot(ctx, snapshot.RawJWS, now); e != nil {
					t.Fatal(e)
				}
				request(spec, "ack", coordination.AckInput{Revision: snapshot.Revision, Digest: snapshot.Digest}, 200)
			}
			server.Close()
			for _, spec := range all {
				rt, e := runtime.NewFileRuntime(paths[spec.ServiceID], runtime.Binding{InstanceID: state.InstanceID, Environment: "production", Application: spec.Application, ServiceID: spec.ServiceID}, platform, vendor)
				if e != nil {
					t.Fatal(e)
				}
				if e = rt.Evaluate(ctx, core.MUTATE_BUSINESS, now.Add(30*time.Minute)); e != nil {
					t.Fatalf("offline valid %s: %v", spec.ServiceID, e)
				}
				if e = rt.Evaluate(ctx, core.MUTATE_BUSINESS, now.Add(time.Hour)); !errors.Is(e, core.ErrDenied) {
					t.Fatalf("offline expiry %s: %v", spec.ServiceID, e)
				}
				for _, op := range []core.Operation{core.READ_HISTORY, core.EXPORT_HISTORY, core.ESSENTIAL_SERVICE} {
					if e = rt.Evaluate(ctx, op, now.Add(time.Hour+time.Second)); e != nil {
						t.Fatalf("expired historical/essential %s %s: %v", spec.ServiceID, op, e)
					}
				}
			}
			t.Logf("protocol evidence: %s, 6 applications, %d independent durable runtimes; all ready/apply/ACK; offline restart/absolute expiry/history passed; Docker/OAuth business acceptance not included", scenario.name, len(all))
		})
	}
}
