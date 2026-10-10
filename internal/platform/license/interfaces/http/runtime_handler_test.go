package licensehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

type runtimeStub struct {
	calls     int
	serviceID string
	actor     domain.Actor
	err       error
}

type evidenceStub struct {
	app, environment string
	err              error
}

func (s *evidenceStub) CollectLicenseEvidence(_ context.Context, app, environment string) (evidence.Report, error) {
	s.app = app
	s.environment = environment
	return evidence.Report{Scope: "APPLICATION", Complete: false, Problems: []string{"evidence_incomplete"}}, s.err
}
func (s *evidenceStub) CollectInstallationLicenseEvidence(context.Context) (evidence.Report, error) {
	return evidence.Report{Scope: "INSTALLATION", BoundarySupported: true, Complete: false}, s.err
}
func TestInstallationEvidenceIsReadOnlyAndPermissionControlled(t *testing.T) {
	h, _ := NewRuntimeHandler(&runtimeStub{})
	r := requestWith("", "platform:license:read")
	r.URL.Path = "/api/v1/licenses/installation-evidence?application=forged"
	if w := invoke(h.InstallationEvidence, r); w.Code != 503 {
		t.Fatal("missing source accepted")
	}
	source := &evidenceStub{}
	h.ConfigureEvidence(source, "prod")
	if w := invoke(h.InstallationEvidence, r); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"scope":"INSTALLATION"`) || source.app != "" {
		t.Fatal("installation action used client selector or lost report scope")
	}
	source.err = errors.New("secret private path")
	if w := invoke(h.InstallationEvidence, r); w.Code != 503 || strings.Contains(w.Body.String(), "private path") {
		t.Fatal("agent error disclosed")
	}
	r = requestWith("", "contract.read")
	if w := invoke(h.InstallationEvidence, r); w.Code != 403 {
		t.Fatal("unauthorized installation evidence")
	}
}
func TestEvidenceRequiresControlledProviderAndRedactsAgentErrors(t *testing.T) {
	h, _ := NewRuntimeHandler(&runtimeStub{})
	r := requestWith("", "platform:license:read")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/evidence"
	if w := invoke(h.Evidence, r); w.Code != 503 || !strings.Contains(w.Body.String(), "LICENSE_EVIDENCE_UNSUPPORTED") {
		t.Fatal("no controlled source accepted")
	}
	source := &evidenceStub{}
	h.ConfigureEvidence(source, "prod")
	if w := invoke(h.Evidence, r); w.Code != 200 || source.app != "contract_management" || source.environment != "prod" || !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatal("evidence semantics changed")
	}
	source.err = errors.New("private host path secret=never-output")
	if w := invoke(h.Evidence, r); w.Code != 503 || strings.Contains(w.Body.String(), "never-output") {
		t.Fatal("agent error disclosed")
	}
	r = requestWith("", "contract.read")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/evidence"
	if w := invoke(h.Evidence, r); w.Code != 403 {
		t.Fatal("business role read installation evidence")
	}
}

func (s *runtimeStub) Ready(_ context.Context, id string, _ coordination.ReadyInput) error {
	s.calls++
	s.serviceID = id
	return s.err
}
func (s *runtimeStub) Snapshot(_ context.Context, id string) (coordination.SnapshotOutput, error) {
	s.calls++
	s.serviceID = id
	return coordination.SnapshotOutput{RawJWS: "signed", Digest: "digest", Revision: 1}, s.err
}
func (s *runtimeStub) Ack(_ context.Context, id string, _ coordination.AckInput) error {
	s.calls++
	s.serviceID = id
	return s.err
}
func (s *runtimeStub) Status(context.Context, string) (coordination.Status, error) {
	s.calls++
	return coordination.Status{}, s.err
}
func (s *runtimeStub) BeginActivation(_ context.Context, _ string, _ uint64, actor domain.Actor) error {
	s.calls++
	s.actor = actor
	return s.err
}
func runtimeRequest(action, body, scope string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/licenses/runtime/contract-api/"+action, strings.NewReader(body))
	if scope != "" {
		r = r.WithContext(appctx.WithPrincipal(r.Context(), appctx.Principal{OAuthClientID: "oauth", ClientID: "client", TenantID: "tenant", ApplicationID: "app", ApplicationCode: "contract_management", EnvironmentID: "env", EnvironmentCode: "test", Scopes: map[string]struct{}{scope: {}}}))
	}
	return r
}
func TestRuntimeTransportUsesMachineNotConsoleIdentity(t *testing.T) {
	service := &runtimeStub{}
	h, err := NewRuntimeHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	for action, fn := range map[string]http.HandlerFunc{"ready": h.Ready, "snapshot": h.Snapshot, "ack": h.Ack} {
		for _, scope := range []string{"", "audit.ingest"} {
			r := runtimeRequest(action, "{}", scope)
			r.Header.Set("X-Service-ID", "contract-api")
			r.Header.Set("X-Scope", "license.runtime")
			w := invoke(fn, r)
			want := 401
			if scope != "" {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s/%s %d", action, scope, w.Code)
			}
		}
		r := requestWith("{}", "platform:license:manage")
		r.URL.Path = "/api/v1/internal/licenses/runtime/contract-api/" + action
		if w := invoke(fn, r); w.Code != 401 {
			t.Fatal("console impersonated service")
		}
	}
	if service.calls != 0 {
		t.Fatal("unauthenticated storage call")
	}
	if w := invoke(h.Ready, runtimeRequest("ready", `{"protocol":1,"coverage_digest":"hash","image_digest":"sha256:hash"}`, "license.runtime")); w.Code != 200 || service.serviceID != "contract-api" {
		t.Fatalf("ready %d %s", w.Code, w.Body.String())
	}
	if w := invoke(h.Snapshot, runtimeRequest("snapshot", "", "license.runtime")); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot caching/binding")
	}
}
func TestRuntimeBodiesRejectEligibilityAndTenantOverrides(t *testing.T) {
	service := &runtimeStub{}
	h, _ := NewRuntimeHandler(service)
	for _, body := range []string{`{"protocol":1,"migration_eligible":true}`, `{"protocol":1,"tenant_id":"other"}`, `{} {}`, strings.Repeat("x", 300000)} {
		if w := invoke(h.Ready, runtimeRequest("ready", body, "license.runtime")); w.Code != 400 {
			t.Fatalf("accepted override %d", w.Code)
		}
	}
	if service.calls != 0 {
		t.Fatal("invalid request reached storage")
	}
	service.err = coordination.ErrForbidden
	if w := invoke(h.Ack, runtimeRequest("ack", `{"revision":1,"digest":"hash"}`, "license.runtime")); w.Code != 403 {
		t.Fatal("binding failure ignored")
	}
}
func TestActivationRequiresPlatformManageAndExpectedRevision(t *testing.T) {
	service := &runtimeStub{}
	h, _ := NewRuntimeHandler(service)
	r := requestWith(`{"expected_revision":2}`, "platform:license:read")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/activation"
	if w := invoke(h.BeginActivation, r); w.Code != 403 {
		t.Fatal("read permission activated")
	}
	r = requestWith(`{"expected_revision":2,"migration_eligible":true}`, "platform:license:manage")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/activation"
	if w := invoke(h.BeginActivation, r); w.Code != 400 {
		t.Fatal("eligibility DTO accepted")
	}
	r = requestWith(`{"expected_revision":2}`, "platform:license:manage")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/activation"
	if w := invoke(h.BeginActivation, r); w.Code != 200 || service.actor.UserID != "admin" {
		t.Fatal("missing platform actor")
	}
	service.err = coordination.ErrNotReady
	if w := invoke(h.BeginActivation, r); w.Code != 400 {
		t.Fatal("consumed request body accepted")
	}
	r = requestWith(`{"expected_revision":2}`, "platform:license:manage")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/activation"
	if w := invoke(h.BeginActivation, r); w.Code != 409 || !strings.Contains(w.Body.String(), "LICENSE_RUNTIME_NOT_READY") {
		t.Fatal("missing incomplete coverage diagnostic")
	}
}
