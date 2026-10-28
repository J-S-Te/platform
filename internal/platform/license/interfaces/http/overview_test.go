package licensehttp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	core "github.com/J-S-Te/license-core"
)

type overviewLicenseStub struct {
	state domain.State
	err   error
}

func (s *overviewLicenseStub) Read(context.Context) (domain.State, error) { return s.state, s.err }

func overviewState(application string, notBefore, expiresAt int64) domain.State {
	return domain.State{
		Revision: 3, CurrentVersion: 1,
		Current: &core.License{Version: 1, NotBefore: notBefore, Applications: []core.Application{{
			Code: application, NotBefore: notBefore, ExpiresAt: expiresAt,
		}}},
	}
}

func overviewRequest() *http.Request {
	r := requestWith("", "platform:license:read")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/overview"
	return r
}

func TestOverviewKeepsUnregisteredApplicationVisible(t *testing.T) {
	now := time.Now().Unix()
	service := &runtimeStub{err: coordination.ErrNotReady}
	h, _ := NewRuntimeHandler(service)
	h.ConfigureOverview(&overviewLicenseStub{state: overviewState("contract_management", now-60, now+3600)}, "prod")
	response := invoke(h.Overview, overviewRequest())
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("overview status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}
	body := response.Body.String()
	for _, want := range []string{`"status":"UNREGISTERED"`, `"status":"VALID"`, `"RUNTIME_NOT_REGISTERED"`, `"registration":{"status":"UNREGISTERED"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview lost %s: %s", want, body)
		}
	}
}

func TestOverviewSeparatesLicenseAndRegistrationGaps(t *testing.T) {
	cases := []struct {
		name       string
		state      domain.State
		stateErr   error
		statusErr  error
		wantStatus string
		wantReason string
	}{
		{"no license", domain.State{}, domain.ErrNotInitialized, coordination.ErrNotReady, "NO_LICENSE", "LICENSE_MISSING"},
		{"not purchased", overviewState("settlement", 0, time.Now().Add(time.Hour).Unix()), nil, coordination.ErrNotReady, "NOT_PURCHASED", "LICENSE_MISSING"},
		{"not effective", overviewState("contract_management", time.Now().Add(time.Hour).Unix(), time.Now().Add(2*time.Hour).Unix()), nil, coordination.ErrNotReady, "NOT_EFFECTIVE", "LICENSE_NOT_EFFECTIVE"},
		{"expired", overviewState("contract_management", 0, time.Now().Add(-time.Minute).Unix()), nil, coordination.ErrNotReady, "EXPIRED", "LICENSE_EXPIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &runtimeStub{err: tc.statusErr}
			h, _ := NewRuntimeHandler(service)
			h.ConfigureOverview(&overviewLicenseStub{state: tc.state, err: tc.stateErr}, "prod")
			body := invoke(h.Overview, overviewRequest()).Body.String()
			wants := []string{`"status":"` + tc.wantStatus + `"`, tc.wantReason}
			if tc.wantStatus != "NOT_EFFECTIVE" {
				wants = append(wants, "IMPORT_LICENSE")
			}
			for _, want := range wants {
				if !strings.Contains(body, want) {
					t.Fatalf("overview lost %s: %s", want, body)
				}
			}
		})
	}
}

func TestOverviewReportsRegisteredEnforcementAndMembers(t *testing.T) {
	now := time.Now().Unix()
	readyAt := time.Unix(now-10, 0)
	service := &runtimeStub{status: coordination.Status{
		Application:        coordination.Application{Application: "contract_management", State: "ENFORCED", Revision: 2, CatalogVersion: 1},
		DeploymentRevision: 4, SnapshotCurrent: true,
		Members: []coordination.Member{{ServiceID: "contract-api", ReadyAt: &readyAt, IssuedRevision: 2, AckRevision: 2}},
	}}
	h, _ := NewRuntimeHandler(service)
	h.ConfigureOverview(&overviewLicenseStub{state: overviewState("contract_management", now-60, now+3600)}, "prod")
	response := invoke(h.Overview, overviewRequest())
	if response.Code != 200 {
		t.Fatalf("overview status=%d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"registration":{"status":"REGISTERED"`, `"member_count":1`, `"status":"ENFORCED"`, `"revision":2`, `"deployment_revision":4`, `"service_id":"contract-api"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview lost %s: %s", want, body)
		}
	}
}

func TestOverviewRequiresReadPermissionAndRejectsUnknownApplication(t *testing.T) {
	service := &runtimeStub{err: coordination.ErrNotReady}
	h, _ := NewRuntimeHandler(service)
	h.ConfigureOverview(&overviewLicenseStub{}, "prod")
	r := requestWith("", "contract.read")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/overview"
	if response := invoke(h.Overview, r); response.Code != 403 || service.calls != 0 {
		t.Fatalf("overview permission=%d calls=%d", response.Code, service.calls)
	}
	service.err = coordination.ErrInvalid
	if response := invoke(h.Overview, overviewRequest()); response.Code != 400 {
		t.Fatalf("unknown application status=%d", response.Code)
	}
}

type overviewObserverStub struct {
	observation licenseevidence.RuntimeObservation
	err         error
}

func (s *overviewObserverStub) ObserveLicenseRuntime(context.Context, string, string) (licenseevidence.RuntimeObservation, error) {
	return s.observation, s.err
}

func TestOverviewIncludesReadOnlyRuntimeObservation(t *testing.T) {
	now := time.Now().Unix()
	service := &runtimeStub{err: coordination.ErrNotReady}
	h, _ := NewRuntimeHandler(service)
	h.ConfigureOverview(&overviewLicenseStub{state: overviewState("contract_management", now-60, now+3600)}, "prod")
	h.ConfigureObserver(&overviewObserverStub{observation: licenseevidence.RuntimeObservation{
		Status: "RUNNING", Available: true, CheckedAt: time.Now().UTC(),
		Components: []licenseevidence.RuntimeComponent{{ServiceID: "contract-api", Exists: true, Running: true, Health: "healthy"}},
	}})
	body := invoke(h.Overview, overviewRequest()).Body.String()
	for _, want := range []string{`"status":"RUNNING"`, `"available":true`, `"service_id":"contract-api"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview lost runtime fact %s: %s", want, body)
		}
	}
}
