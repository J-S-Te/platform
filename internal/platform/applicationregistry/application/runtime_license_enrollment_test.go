package application

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"strings"
	"testing"
)

type runtimeApprovalsStub struct {
	items []RuntimeLicenseApproval
	err   error
}
type runtimeLifecycleSourceStub struct {
	runtimeApprovalsStub
	release RuntimeLicenseReleaseApproval
}

func (s runtimeLifecycleSourceStub) ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (RuntimeLicenseReleaseApproval, error) {
	return s.release, s.err
}

type runtimeLifecycleRegistrarStub struct {
	runtimeRegistrarStub
	status   coordination.Status
	requests []coordination.LifecycleInput
}

func (s *runtimeLifecycleRegistrarStub) Status(context.Context, string) (coordination.Status, error) {
	return s.status, nil
}
func (s *runtimeLifecycleRegistrarStub) ReconcileComponents(_ context.Context, in coordination.LifecycleInput, _ domain.Actor) error {
	s.requests = append(s.requests, in)
	return s.err
}

func TestRuntimeEnrollmentExplicitReplacementCASAndNoPrematureRotation(t *testing.T) {
	items := testRuntimeApprovals()
	source := runtimeLifecycleSourceStub{runtimeApprovalsStub: runtimeApprovalsStub{items: items}, release: RuntimeLicenseReleaseApproval{Components: items, RequiredServiceIDs: []string{"contract-api", "contract-worker"}}}
	clients := &runtimeClientsStub{}
	registrar := &runtimeLifecycleRegistrarStub{status: coordination.Status{Application: coordination.Application{Application: "contract_management", Revision: 17}, Members: []coordination.Member{{ServiceID: "contract-api", Application: "contract_management", Environment: "prod", OAuthClientID: "old-client", CoverageDigest: items[0].CoverageDigest, ImageDigest: items[0].ImageDigest}}}}
	service, _ := NewRuntimeLicenseEnrollmentService(source, clients, registrar)
	output, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator")
	if err != nil || len(output) != 2 || len(registrar.requests) != 1 || len(registrar.specs) != 0 {
		t.Fatal("replacement did not use full CAS", err)
	}
	in := registrar.requests[0]
	if in.ExpectedRevision != 17 || in.Environment != "prod" || in.ReleaseDigest == "" {
		t.Fatal("missing controlled binding")
	}
	for _, credential := range output {
		if credential.ReleaseDigest != in.ReleaseDigest {
			t.Fatal("delivery lost CAS approval binding")
		}
	}
	registrar.err = coordination.ErrConflict
	if _, err = service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); err == nil || clients.rotations != 0 {
		t.Fatal("CAS conflict invalidated working credential")
	}
	withoutApproval, _ := NewRuntimeLicenseEnrollmentService(runtimeApprovalsStub{items: items}, clients, registrar)
	registrar.err = nil
	if _, err = withoutApproval.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); err == nil {
		t.Fatal("unapproved replacement accepted")
	}
}
func TestRuntimeReleaseDigestBindsRetirementAndGeneration(t *testing.T) {
	base := RuntimeLicenseReleaseApproval{Components: testRuntimeApprovals(), RequiredServiceIDs: []string{"contract-api"}}
	original, err := RuntimeLicenseReleaseDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.RetireServiceIDs = []string{"old-contract-worker"}
	retired, err := RuntimeLicenseReleaseDigest(changed)
	if err != nil || retired == original {
		t.Fatal("retirement omitted from delivery approval", err)
	}
	changed = base
	changed.ReleaseGeneration = "rollback-v3"
	generation, err := RuntimeLicenseReleaseDigest(changed)
	if err != nil || generation == original {
		t.Fatal("generation omitted from delivery approval", err)
	}
	changed = base
	changed.RequiredServiceIDs = []string{"contract-api", "contract-worker"}
	required, err := RuntimeLicenseReleaseDigest(changed)
	if err != nil || required == original {
		t.Fatal("required membership omitted from delivery approval", err)
	}
}
func TestRuntimeReleaseVerifierRejectsMutableApprovalAndClientGeneration(t *testing.T) {
	items := testRuntimeApprovals()
	source := runtimeLifecycleSourceStub{runtimeApprovalsStub: runtimeApprovalsStub{items: items}, release: RuntimeLicenseReleaseApproval{Components: items, RequiredServiceIDs: []string{"contract-api"}}}
	verifier, err := NewRuntimeLicenseLifecycleVerifier(source)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := canonicalRuntimeRelease(source.release)
	if err != nil {
		t.Fatal(err)
	}
	in := coordination.LifecycleInput{Application: "contract_management", Environment: "prod", ExpectedRevision: 1, OperationID: "release1", ReleaseDigest: digest}
	approved, err := verifier.VerifyRuntimeLifecycle(context.Background(), in)
	if err != nil || len(approved.Specs) != 2 {
		t.Fatal(err)
	}
	in.ReleaseDigest = "sha256:" + strings.Repeat("f", 64)
	if _, err = verifier.VerifyRuntimeLifecycle(context.Background(), in); !errors.Is(err, coordination.ErrForbidden) {
		t.Fatal("changed release approval accepted")
	}
	id := RuntimeLicenseClientID("contract_management", "prod", items[0])
	changed := items[0]
	changed.ImageDigest = "sha256:" + strings.Repeat("f", 64)
	if len(id) > 128 || id == RuntimeLicenseClientID("contract_management", "prod", changed) || id != RuntimeLicenseClientID("contract_management", "prod", items[0]) {
		t.Fatal("unstable/unbounded client generation")
	}
	if len(RuntimeLicenseClientID(strings.Repeat("a", 64), strings.Repeat("e", 64), items[0])) > 128 {
		t.Fatal("long component client exceeds schema")
	}
}

func (s runtimeApprovalsStub) ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]RuntimeLicenseApproval, error) {
	return s.items, s.err
}

type runtimeClientsStub struct {
	clients   []OAuthClientView
	created   []OAuthClientCreateInput
	rotations int
}

func (s *runtimeClientsStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return s.clients, nil
}
func (s *runtimeClientsStub) CreateOAuthClient(_ context.Context, in OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	s.created = append(s.created, in)
	c := OAuthClientView{ID: in.ClientID, ClientID: in.ClientID, ApplicationID: in.ApplicationID, EnvironmentID: in.EnvironmentID, ClientType: in.ClientType, TokenAuthMethod: in.TokenAuthMethod, GrantTypes: in.GrantTypes, Scopes: in.Scopes, Status: "ACTIVE"}
	s.clients = append(s.clients, c)
	return OAuthClientCreateResult{Client: c, PlaintextSecret: "isolated-" + in.ClientID}, nil
}
func (s *runtimeClientsStub) CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	s.rotations++
	return OAuthClientSecretResult{PlaintextSecret: "replacement"}, nil
}

type runtimeRegistrarStub struct {
	specs []coordination.ServiceSpec
	err   error
}

func (s *runtimeRegistrarStub) Register(_ context.Context, spec coordination.ServiceSpec) error {
	s.specs = append(s.specs, spec)
	return s.err
}
func testRuntimeApprovals() []RuntimeLicenseApproval {
	return []RuntimeLicenseApproval{{ServiceID: "contract-api", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)}, {ServiceID: "contract-worker", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("d", 64)}}
}
func TestRuntimeEnrollmentComponentIsolationAndRetry(t *testing.T) {
	clients := &runtimeClientsStub{}
	registrar := &runtimeRegistrarStub{}
	service, _ := NewRuntimeLicenseEnrollmentService(runtimeApprovalsStub{items: testRuntimeApprovals()}, clients, registrar)
	items, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator")
	if err != nil || len(items) != 2 {
		t.Fatalf("prepare count %d err %v", len(items), err)
	}
	if items[0].ClientID == items[1].ClientID || items[0].ClientSecret == items[1].ClientSecret {
		t.Fatal("component credential reuse")
	}
	for _, in := range clients.created {
		if len(in.Scopes) != 1 || in.Scopes[0] != "license.runtime" || in.ClientType != "service" || in.TokenAuthMethod != "client_secret_basic" || len(in.GrantTypes) != 1 || in.GrantTypes[0] != "client_credentials" {
			t.Fatal("runtime client not least privilege")
		}
	}
	_, err = service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator")
	if err != nil || len(clients.created) != 2 || clients.rotations != 2 || len(registrar.specs) != 4 {
		t.Fatal("retry recreated client or skipped registration")
	}
}
func TestRuntimeEnrollmentRejectsConflictBeforeRotation(t *testing.T) {
	clients := &runtimeClientsStub{}
	registrar := &runtimeRegistrarStub{}
	service, _ := NewRuntimeLicenseEnrollmentService(runtimeApprovalsStub{items: testRuntimeApprovals()}, clients, registrar)
	_, _ = service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator")
	registrar.err = errors.New("binding conflict")
	if _, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); err == nil || clients.rotations != 0 {
		t.Fatal("registration failure rotated live credentials")
	}
	registrar.err = nil
	clients.clients[0].Scopes = append(clients.clients[0].Scopes, "audit.ingest")
	if _, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted overprivileged client")
	}
}
func TestRuntimeEnrollmentInvalidApprovalHasNoWrites(t *testing.T) {
	for _, items := range [][]RuntimeLicenseApproval{nil, {{ServiceID: "../secret", Protocol: 1}}, append(testRuntimeApprovals(), testRuntimeApprovals()[0])} {
		clients := &runtimeClientsStub{}
		registrar := &runtimeRegistrarStub{}
		service, _ := NewRuntimeLicenseEnrollmentService(runtimeApprovalsStub{items: items}, clients, registrar)
		if _, err := service.Prepare(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator"); err == nil || len(clients.created) > 0 || len(registrar.specs) > 0 {
			t.Fatal("invalid approval mutated control plane")
		}
	}
}
