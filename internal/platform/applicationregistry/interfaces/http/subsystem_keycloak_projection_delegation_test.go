package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type delegationCatalogStub struct {
	roles []string
	err   error
	calls int
}

func (stub *delegationCatalogStub) ListKeycloakRoleCodes(context.Context, string, string) ([]string, error) {
	stub.calls++
	return stub.roles, stub.err
}

type delegationControlStub struct{ ensured []string }

func (stub *delegationControlStub) EnsureBroker(context.Context, string, string) error { return nil }
func (stub *delegationControlStub) VerifyBrokerExists(context.Context) error           { return nil }
func (stub *delegationControlStub) EnsureClient(context.Context, string, string, string) (keycloakClientResult, error) {
	return keycloakClientResult{}, nil
}
func (stub *delegationControlStub) EnsureClientRoles(_ context.Context, _ string, roles []string) error {
	stub.ensured = roles
	return nil
}
func (stub *delegationControlStub) DetectSubsystemKeycloakDrift(context.Context, string, string, []string) (KeycloakDriftReport, error) {
	return KeycloakDriftReport{}, nil
}

type delegationMappingStub struct {
	saved, backfilled int
	saveErr           error
}

func (stub *delegationMappingStub) SaveKeycloakClientMapping(context.Context, string, string, string, string, string) error {
	stub.saved++
	return stub.saveErr
}
func (stub *delegationMappingStub) BackfillKeycloakAuthorization(context.Context, string, string, string) error {
	stub.backfilled++
	return nil
}

// 旧入口必须调用共享应用服务：角色目录读取 → 确保角色 → 保存映射 → 回填投影。
// 这样新旧入口就不会各维护一份投影流程。
func TestHandlerProjectionDelegatesToSharedService(t *testing.T) {
	catalog := &delegationCatalogStub{roles: []string{"crm.admin"}}
	control := &delegationControlStub{}
	mappings := &delegationMappingStub{}
	handler := &SubsystemOnboardingHandler{
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		keycloakCatalog:  catalog,
		keycloakControl:  control,
		keycloakMappings: mappings,
		keycloakRealm:    "basic",
	}
	job := subsystemDeploymentJob{KeycloakClientID: "settlement-dev-web", TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1"}
	if err := handler.completeOnboardKeycloakProjection(context.Background(), job); err != nil {
		t.Fatalf("projection: %v", err)
	}
	if catalog.calls != 1 || mappings.saved != 1 || mappings.backfilled != 1 {
		t.Fatalf("calls catalog=%d saved=%d backfilled=%d，必须走共享服务完整链路", catalog.calls, mappings.saved, mappings.backfilled)
	}
	if len(control.ensured) != 1 || control.ensured[0] != "crm.admin" {
		t.Fatalf("ensured roles = %v", control.ensured)
	}
}

// 映射保存失败：必须以共享服务的稳定错误码收口，不能静默返回成功。
func TestHandlerProjectionReportsSharedServiceCode(t *testing.T) {
	mappings := &delegationMappingStub{saveErr: errors.New("conflict")}
	handler := &SubsystemOnboardingHandler{
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		keycloakCatalog:  &delegationCatalogStub{},
		keycloakControl:  &delegationControlStub{},
		keycloakMappings: mappings,
	}
	job := subsystemDeploymentJob{KeycloakClientID: "c", TenantID: "t", ApplicationID: "a", EnvironmentID: "e"}
	err := handler.completeOnboardKeycloakProjection(context.Background(), job)
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("err = %v，want ErrSubsystemProvisioningUnavailable", err)
	}
}
