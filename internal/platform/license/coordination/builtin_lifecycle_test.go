package coordination

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/license-core/catalog"
)

type builtinSourceStub struct {
	components []catalog.Component
	err        error
}

func (s builtinSourceStub) BuiltinComponents(context.Context, string, string) ([]catalog.Component, error) {
	return s.components, s.err
}

func validBuiltinInput() BuiltinReconcileInput {
	return BuiltinReconcileInput{
		Application: "contract_management", Environment: "prod", OperationID: "builtin-op",
		ExpectedRevision: 3, CatalogVersion: catalog.Version, CredentialGeneration: "catalog-1",
	}
}

func TestBuiltinReconcileRejectsUnsafeCatalogAndInput(t *testing.T) {
	actor := domain.Actor{TenantID: "tenant", UserID: "operator"}
	if err := (&Service{}).ReconcileBuiltinComponents(context.Background(), validBuiltinInput(), actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing source error = %v", err)
	}
	validComponents := []catalog.Component{
		{Application: "contract_management", ServiceID: "contract-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2},
		{Application: "contract_management", ServiceID: "contract-worker", Role: catalog.RoleWorker, RegistrationProtocol: catalog.RegistrationProtocolV2},
	}
	service := &Service{builtinCatalog: builtinSourceStub{components: validComponents}}
	for _, mutate := range []func(*BuiltinReconcileInput){
		func(in *BuiltinReconcileInput) { in.ExpectedRevision = 0 },
		func(in *BuiltinReconcileInput) { in.CatalogVersion = 2 },
		func(in *BuiltinReconcileInput) { in.OperationID = "" },
		func(in *BuiltinReconcileInput) { in.CredentialGeneration = "" },
	} {
		in := validBuiltinInput()
		mutate(&in)
		if err := service.ReconcileBuiltinComponents(context.Background(), in, actor); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input %+v error = %v", in, err)
		}
	}
	bad := &Service{builtinCatalog: builtinSourceStub{components: append(validComponents, catalog.Component{Application: "data_analysis", ServiceID: "data-analysis-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2})}}
	if err := bad.ReconcileBuiltinComponents(context.Background(), validBuiltinInput(), actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-application component error = %v", err)
	}
}

func TestBuiltinBusinessComponentsSkipsInfrastructure(t *testing.T) {
	components := []catalog.Component{
		{Application: "data_analysis", ServiceID: "data-analysis-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-aggregation-worker", Role: catalog.RoleWorker, RegistrationProtocol: catalog.RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-alert-worker", Role: catalog.RoleWorker, RegistrationProtocol: catalog.RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-metabase", Role: catalog.RoleInfrastructure, RegistrationProtocol: catalog.RegistrationProtocolV2},
	}
	got, err := builtinBusinessComponents("data_analysis", components)
	if err != nil {
		t.Fatalf("filter infrastructure: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("business components = %d, want 3", len(got))
	}
	for _, component := range got {
		if component.Role == catalog.RoleInfrastructure {
			t.Fatalf("infrastructure component was not filtered: %s", component.ServiceID)
		}
	}
}

func TestBuiltinBusinessComponentsRejectsInvalidComponents(t *testing.T) {
	valid := catalog.Component{Application: "data_analysis", ServiceID: "data-analysis-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2}
	cases := [][]catalog.Component{
		{{Application: "other", ServiceID: "data-analysis-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2}},
		{{Application: "data_analysis", ServiceID: "data-analysis-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV1}},
		{valid, valid},
	}
	for _, components := range cases {
		if _, err := builtinBusinessComponents("data_analysis", components); !errors.Is(err, ErrInvalid) {
			t.Fatalf("components %+v error = %v, want ErrInvalid", components, err)
		}
	}
}
