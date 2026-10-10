package coordination

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

type fixtureApproval struct{ value LifecycleApproval }

func (f fixtureApproval) VerifyRuntimeLifecycle(context.Context, LifecycleInput) (LifecycleApproval, error) {
	return f.value, nil
}
func TestRetirementCannotCrossApplicationOrEnvironment(t *testing.T) {
	in := LifecycleInput{Application: "contract_management", Environment: "prod"}
	m := Member{Application: in.Application, Environment: in.Environment}
	if !validRetirementOwner(in, m, false) || validRetirementOwner(in, m, true) {
		t.Fatal("active member/tombstone ownership")
	}
	now := time.Now()
	m.RetiredAt = &now
	if !validRetirementOwner(in, m, true) {
		t.Fatal("same-application tombstone rejected")
	}
	m.Application = "customer_and_opportunity"
	if validRetirementOwner(in, m, false) || validRetirementOwner(in, m, true) {
		t.Fatal("cross-application retirement allowed")
	}
	m.Application = in.Application
	m.Environment = "production"
	if validRetirementOwner(in, m, false) || validRetirementOwner(in, m, true) {
		t.Fatal("cross-environment retirement allowed")
	}
}
func TestLifecycleApprovalValidation(t *testing.T) {
	spec := ServiceSpec{Application: "contract_management", Environment: "production", ServiceID: "contract-api", OAuthClientID: "new-client", CoverageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ImageDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	in := LifecycleInput{Application: spec.Application, Environment: spec.Environment, ExpectedRevision: 1, OperationID: "release-v2", ReleaseDigest: spec.ImageDigest}
	approval := LifecycleApproval{Specs: []ServiceSpec{spec}, RequiredServiceIDs: []string{spec.ServiceID}}
	if _, err := canonicalApproval(in, approval); err != nil {
		t.Fatal(err)
	}
	cases := []LifecycleApproval{
		{Specs: []ServiceSpec{spec}},
		{Specs: []ServiceSpec{spec}, RequiredServiceIDs: []string{"missing"}},
		{Specs: []ServiceSpec{spec, spec}, RequiredServiceIDs: []string{spec.ServiceID}},
		{Specs: []ServiceSpec{spec}, RequiredServiceIDs: []string{spec.ServiceID}, RetireServiceIDs: []string{spec.ServiceID}},
	}
	for i, bad := range cases {
		if _, err := canonicalApproval(in, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid approval %d accepted: %v", i, err)
		}
	}
	zero := in
	zero.ExpectedRevision = 0
	if _, err := canonicalApproval(zero, approval); !errors.Is(err, ErrInvalid) {
		t.Fatal("no revision accepted")
	}
}
func TestLifecycleIsNotMachineOrUnapprovedEntry(t *testing.T) {
	actor := domain.Actor{TenantID: "tenant", UserID: "operator"}
	s := &Service{}
	if !errors.Is(s.ReconcileComponents(context.Background(), LifecycleInput{}, actor), ErrForbidden) {
		t.Fatal("unapproved lifecycle accepted")
	}
	s.lifecycleApproval = fixtureApproval{}
	ctx := appctx.WithPrincipal(context.Background(), appctx.Principal{ClientID: "client", OAuthClientID: "client", TenantID: "tenant", ApplicationID: "app", ApplicationCode: "contract_management", EnvironmentID: "env", EnvironmentCode: "prod", Scopes: map[string]struct{}{"license.runtime": {}}})
	if !errors.Is(s.ReconcileComponents(ctx, LifecycleInput{}, actor), ErrForbidden) {
		t.Fatal("machine lifecycle accepted")
	}
}
