package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/license-core/catalog"
)

type builtinCatalogStub struct{}

func (builtinCatalogStub) RuntimeComponentSet(context.Context, string, string) (RuntimeComponentSet, error) {
	return RuntimeComponentSet{
		Application: "contract_management", Environment: "prod", CatalogVersion: 1,
		Components: []RuntimeComponentSpec{
			{ServiceID: "contract-api", RegistrationProtocol: 2, CatalogVersion: 1, CredentialGeneration: "catalog-1"},
			{ServiceID: "contract-worker", RegistrationProtocol: 2, CatalogVersion: 1, CredentialGeneration: "catalog-1"},
		},
		RequiredServiceIDs: []string{"contract-api", "contract-worker"},
	}, nil
}

type builtinRegistrarStub struct {
	specs []coordination.ServiceSpec
}

func (s *builtinRegistrarStub) Register(_ context.Context, spec coordination.ServiceSpec) error {
	s.specs = append(s.specs, spec)
	return nil
}
func (s *builtinRegistrarStub) Status(context.Context, string) (coordination.Status, error) {
	return coordination.Status{}, coordination.ErrNotReady
}

func TestBuiltinEnrollmentUsesCatalogWithoutApprovalDirectory(t *testing.T) {
	clients := &runtimeClientsStub{}
	registrar := &builtinRegistrarStub{}
	service, err := NewRuntimeLicenseEnrollmentServiceWithCatalog(nil, builtinCatalogStub{}, clients, registrar)
	if err != nil {
		t.Fatal(err)
	}
	output, err := service.PrepareWithSettings(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", RuntimeLicenseSettings{InstanceID: "instance-1"})
	if err != nil || len(output) != 2 || len(registrar.specs) != 2 {
		t.Fatalf("builtin prepare = %d/%d/%v", len(output), len(registrar.specs), err)
	}
	for _, credential := range output {
		if credential.RegistrationProtocol != 2 || credential.CatalogVersion != 1 || credential.CredentialGeneration != "catalog-1" || credential.Approval.CoverageDigest != "" || credential.Approval.ImageDigest != "" || credential.ClientID == "" || credential.ClientSecret == "" {
			t.Fatalf("invalid builtin credential: %+v", credential)
		}
	}
	for _, spec := range registrar.specs {
		if spec.RegistrationProtocol != 2 || spec.CatalogVersion != 1 || spec.CredentialGeneration != "catalog-1" || spec.CoverageDigest != "" || spec.ImageDigest != "" {
			t.Fatalf("invalid builtin spec: %+v", spec)
		}
	}
	if len(clients.created) != 2 || clients.created[0].ClientID == clients.created[1].ClientID {
		t.Fatal("builtin clients were not independently created")
	}
}

type builtinBridgeRegistrarStub struct {
	input coordination.BuiltinReconcileInput
}

func (s *builtinBridgeRegistrarStub) Register(context.Context, coordination.ServiceSpec) error {
	return errors.New("bridge must not use Register")
}
func (s *builtinBridgeRegistrarStub) Status(context.Context, string) (coordination.Status, error) {
	return coordination.Status{Application: coordination.Application{Application: "contract_management", Revision: 3, RegistrationMode: catalog.BridgingComponentV2, CatalogVersion: catalog.Version}}, nil
}
func (s *builtinBridgeRegistrarStub) ReconcileBuiltinComponents(_ context.Context, input coordination.BuiltinReconcileInput, _ domain.Actor) error {
	s.input = input
	return nil
}

func TestBuiltinEnrollmentBridgesExistingApplicationViaCatalog(t *testing.T) {
	clients := &runtimeClientsStub{}
	registrar := &builtinBridgeRegistrarStub{}
	service, err := NewRuntimeLicenseEnrollmentServiceWithCatalog(nil, builtinCatalogStub{}, clients, registrar)
	if err != nil {
		t.Fatal(err)
	}
	output, err := service.PrepareWithSettings(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", RuntimeLicenseSettings{InstanceID: "instance-1"})
	if err != nil || len(output) != 2 {
		t.Fatalf("bridge prepare = %d/%v", len(output), err)
	}
	if registrar.input.Application != "contract_management" || registrar.input.Environment != "prod" || registrar.input.ExpectedRevision != 3 || registrar.input.CatalogVersion != catalog.Version || registrar.input.CredentialGeneration != "catalog-1" || registrar.input.OperationID == "" {
		t.Fatalf("bridge input = %+v", registrar.input)
	}
}

func TestBuiltinEnrollmentBridgesSelectedMemberOnly(t *testing.T) {
	clients := &runtimeClientsStub{}
	registrar := &builtinBridgeRegistrarStub{}
	service, err := NewRuntimeLicenseEnrollmentServiceWithCatalog(nil, builtinCatalogStub{}, clients, registrar)
	if err != nil {
		t.Fatal(err)
	}
	output, err := service.PrepareWithSettings(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", RuntimeLicenseSettings{InstanceID: "instance-1", ServiceIDs: []string{"contract-worker"}})
	if err != nil || len(output) != 1 || output[0].Approval.ServiceID != "contract-worker" {
		t.Fatalf("partial prepare = %+v/%v", output, err)
	}
	if len(registrar.input.ServiceIDs) != 1 || registrar.input.ServiceIDs[0] != "contract-worker" || len(clients.created) != 1 {
		t.Fatalf("partial bridge input = %+v, clients=%d", registrar.input, len(clients.created))
	}
}

func TestBuiltinEnrollmentRequiresInstanceBinding(t *testing.T) {
	service, err := NewRuntimeLicenseEnrollmentServiceWithCatalog(nil, builtinCatalogStub{}, &runtimeClientsStub{}, &builtinRegistrarStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareWithSettings(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", RuntimeLicenseSettings{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing instance error = %v", err)
	}
}

type builtinMembersRegistrar struct {
	builtinBridgeRegistrarStub
	members []coordination.Member
}

func (s *builtinMembersRegistrar) Status(context.Context, string) (coordination.Status, error) {
	active := []coordination.Member{}
	for _, member := range s.members {
		if member.RetiredAt == nil {
			active = append(active, member)
		}
	}
	return coordination.Status{Application: coordination.Application{Application: "contract_management", RegistrationMode: catalog.BuiltinComponentV2, Revision: 3}, Members: active}, nil
}
func TestBuiltinRetirementProofRequiresNoRemovedMembers(t *testing.T) {
	for _, removed := range []bool{false, true} {
		registrar := &builtinMembersRegistrar{members: []coordination.Member{{ServiceID: "contract-api"}, {ServiceID: "contract-worker"}}}
		if removed {
			registrar.members = append(registrar.members, coordination.Member{ServiceID: "contract-old-worker"})
		}
		service, err := NewRuntimeLicenseEnrollmentServiceWithCatalog(nil, builtinCatalogStub{}, &runtimeClientsStub{}, registrar)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			out, err := service.PrepareWithSettings(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", RuntimeLicenseSettings{InstanceID: "instance-1"})
			if err != nil {
				t.Fatal(err)
			}
			for _, credential := range out {
				if credential.RetirementNotRequired == removed {
					t.Fatalf("unsafe retirement proof removed=%v", removed)
				}
			}
		}
	}
}

func (s *builtinMembersRegistrar) ReconcileBuiltinComponents(context.Context, coordination.BuiltinReconcileInput, domain.Actor) error {
	now := time.Now()
	for i := range s.members {
		if s.members[i].ServiceID == "contract-old-worker" {
			s.members[i].RetiredAt = &now
		}
	}
	return nil
}
func (s *builtinBridgeRegistrarStub) RuntimeRetirementNotRequired(context.Context, string, uint64, []string) (bool, error) {
	return true, nil
}
func (s *builtinMembersRegistrar) RuntimeRetirementNotRequired(_ context.Context, _ string, _ uint64, ids []string) (bool, error) {
	for _, member := range s.members {
		found := false
		for _, id := range ids {
			if id == member.ServiceID {
				found = true
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}
