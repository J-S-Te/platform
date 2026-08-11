package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type applicationDeleteRepositoryStub struct {
	current                Application
	getErr                 error
	updateErr              error
	updateCalls            int
	updateInput            ApplicationUpdateInput
	updatedAt              time.Time
	environment            Environment
	environmentErr         error
	deleteEnvironmentErr   error
	deleteEnvironmentCalls int
	deleteEnvironmentInput EnvironmentDeleteInput
}

func (repository *applicationDeleteRepositoryStub) ListApplications(context.Context, string, PageRequest) (PageResult[Application], error) {
	return PageResult[Application]{}, nil
}

func (repository *applicationDeleteRepositoryStub) CreateApplication(context.Context, ApplicationCreateInput, string, time.Time) (Application, error) {
	return Application{}, nil
}

func (repository *applicationDeleteRepositoryStub) GetApplication(context.Context, string, string) (Application, error) {
	if repository.getErr != nil {
		return Application{}, repository.getErr
	}
	return repository.current, nil
}

func (repository *applicationDeleteRepositoryStub) UpdateApplication(_ context.Context, input ApplicationUpdateInput, updatedAt time.Time) (Application, error) {
	repository.updateCalls++
	repository.updateInput = input
	repository.updatedAt = updatedAt
	if repository.updateErr != nil {
		return Application{}, repository.updateErr
	}
	result := repository.current
	result.Name = input.Name
	result.ApplicationType = input.ApplicationType
	result.OwnerOrgID = input.OwnerOrgID
	result.OwnerUserID = input.OwnerUserID
	result.HomepageURL = input.HomepageURL
	result.Description = input.Description
	result.Status = input.Status
	result.Version++
	result.UpdatedAt = updatedAt
	return result, nil
}

func (repository *applicationDeleteRepositoryStub) ListEnvironments(context.Context, string, string, PageRequest) (PageResult[Environment], error) {
	return PageResult[Environment]{}, nil
}

func (repository *applicationDeleteRepositoryStub) CreateEnvironment(context.Context, EnvironmentCreateInput, string, time.Time) (Environment, error) {
	return Environment{}, nil
}

func (repository *applicationDeleteRepositoryStub) GetEnvironment(context.Context, string, string, string) (Environment, error) {
	if repository.environmentErr != nil {
		return Environment{}, repository.environmentErr
	}
	return repository.environment, nil
}

func (repository *applicationDeleteRepositoryStub) UpdateEnvironment(context.Context, EnvironmentUpdateInput, time.Time) (Environment, error) {
	return Environment{}, nil
}

func (repository *applicationDeleteRepositoryStub) DeleteEnvironment(_ context.Context, input EnvironmentDeleteInput) (Environment, error) {
	repository.deleteEnvironmentCalls++
	repository.deleteEnvironmentInput = input
	if repository.deleteEnvironmentErr != nil {
		return Environment{}, repository.deleteEnvironmentErr
	}
	return repository.environment, nil
}

type applicationDeleteIDGenerator struct{}

func (applicationDeleteIDGenerator) New(time.Time) (string, error) { return "unused", nil }

type applicationDeleteClock struct{ now time.Time }

func (clock applicationDeleteClock) Now() time.Time { return clock.now }

func newApplicationDeleteService(t *testing.T, repository *applicationDeleteRepositoryStub) *ManagementService {
	t.Helper()
	service, err := NewManagementService(
		repository,
		applicationDeleteIDGenerator{},
		applicationDeleteClock{now: time.Date(2026, time.July, 28, 8, 30, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}
	return service
}

func contractApplicationForDelete() Application {
	description := "合同台账、审批与归档"
	return Application{
		ID:              "01K1APP000000000000000001",
		TenantID:        "01K1TENANT00000000000001",
		Code:            "contract-management",
		Name:            "合同管理系统",
		ApplicationType: "WEB",
		Description:     &description,
		Status:          "ACTIVE",
		Version:         3,
	}
}

func contractProductionEnvironmentForDelete() Environment {
	return Environment{
		ID:            "01K1ENV000000000000000001",
		TenantID:      "01K1TENANT00000000000001",
		ApplicationID: "01K1APP000000000000000001",
		Environment:   "prod",
		Status:        "ACTIVE",
		Version:       5,
	}
}

func TestDeleteEnvironmentRemovesOnlyConfirmedNonDevelopmentEnvironment(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{
		current:     contractApplicationForDelete(),
		environment: contractProductionEnvironmentForDelete(),
	}
	service := newApplicationDeleteService(t, repository)

	removed, err := service.DeleteEnvironment(context.Background(), EnvironmentDeleteInput{
		TenantID:         repository.current.TenantID,
		OperatorID:       "01K1USER0000000000000001",
		ApplicationID:    repository.current.ID,
		EnvironmentID:    repository.environment.ID,
		ConfirmationCode: repository.current.Code + "/" + repository.environment.Environment,
		Version:          repository.environment.Version,
	})
	if err != nil {
		t.Fatalf("delete environment: %v", err)
	}
	if repository.deleteEnvironmentCalls != 1 {
		t.Fatalf("expected one environment deletion, got %d", repository.deleteEnvironmentCalls)
	}
	if repository.deleteEnvironmentInput.ConfirmationCode != "contract-management/prod" {
		t.Fatalf("unexpected confirmation: %q", repository.deleteEnvironmentInput.ConfirmationCode)
	}
	if removed.ID != repository.environment.ID || removed.Environment != "prod" {
		t.Fatalf("unexpected removed environment: %#v", removed)
	}
}

func TestDeleteEnvironmentRejectsDevelopmentEnvironment(t *testing.T) {
	environment := contractProductionEnvironmentForDelete()
	environment.Environment = "dev"
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete(), environment: environment}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteEnvironment(context.Background(), EnvironmentDeleteInput{
		TenantID: repository.current.TenantID, OperatorID: "operator-1", ApplicationID: repository.current.ID,
		EnvironmentID: environment.ID, ConfirmationCode: repository.current.Code + "/dev", Version: environment.Version,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if repository.deleteEnvironmentCalls != 0 {
		t.Fatalf("development environment must not be deleted, got %d calls", repository.deleteEnvironmentCalls)
	}
}

func TestDeleteEnvironmentRequiresExactScopedConfirmation(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete(), environment: contractProductionEnvironmentForDelete()}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteEnvironment(context.Background(), EnvironmentDeleteInput{
		TenantID: repository.current.TenantID, OperatorID: "operator-1", ApplicationID: repository.current.ID,
		EnvironmentID: repository.environment.ID, ConfirmationCode: repository.current.Code, Version: repository.environment.Version,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if repository.deleteEnvironmentCalls != 0 {
		t.Fatalf("mismatched confirmation must not delete, got %d calls", repository.deleteEnvironmentCalls)
	}
}

func TestDeleteEnvironmentRejectsStaleVersion(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete(), environment: contractProductionEnvironmentForDelete()}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteEnvironment(context.Background(), EnvironmentDeleteInput{
		TenantID: repository.current.TenantID, OperatorID: "operator-1", ApplicationID: repository.current.ID,
		EnvironmentID: repository.environment.ID, ConfirmationCode: repository.current.Code + "/prod", Version: repository.environment.Version - 1,
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	if repository.deleteEnvironmentCalls != 0 {
		t.Fatalf("stale version must not delete, got %d calls", repository.deleteEnvironmentCalls)
	}
}

func TestDeleteApplicationRetiresRegistrationAndPreservesMutableFields(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete()}
	service := newApplicationDeleteService(t, repository)

	retired, err := service.DeleteApplication(context.Background(), ApplicationDeleteInput{
		TenantID:         repository.current.TenantID,
		OperatorID:       "01K1USER0000000000000001",
		ApplicationID:    repository.current.ID,
		ConfirmationCode: repository.current.Code,
		Version:          repository.current.Version,
	})
	if err != nil {
		t.Fatalf("delete application: %v", err)
	}
	if repository.updateCalls != 1 {
		t.Fatalf("expected one logical-delete update, got %d", repository.updateCalls)
	}
	if repository.updateInput.Status != "RETIRED" {
		t.Fatalf("expected RETIRED status, got %q", repository.updateInput.Status)
	}
	if repository.updateInput.Name != repository.current.Name || repository.updateInput.ApplicationType != repository.current.ApplicationType {
		t.Fatalf("mutable application fields were not preserved: %#v", repository.updateInput)
	}
	if repository.updateInput.Description != repository.current.Description {
		t.Fatal("description pointer should be preserved during logical deletion")
	}
	if retired.Status != "RETIRED" || retired.Version != repository.current.Version+1 {
		t.Fatalf("unexpected retired application: %#v", retired)
	}
}

func TestDeleteApplicationRejectsBuiltInPlatform(t *testing.T) {
	current := contractApplicationForDelete()
	current.Code = builtInApplicationCode
	repository := &applicationDeleteRepositoryStub{current: current}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteApplication(context.Background(), ApplicationDeleteInput{
		TenantID: current.TenantID, OperatorID: "operator-1", ApplicationID: current.ID,
		ConfirmationCode: current.Code, Version: current.Version,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if repository.updateCalls != 0 {
		t.Fatalf("built-in platform must not be updated, got %d calls", repository.updateCalls)
	}
}

func TestDeleteApplicationRequiresExactStableCodeConfirmation(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete()}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteApplication(context.Background(), ApplicationDeleteInput{
		TenantID: repository.current.TenantID, OperatorID: "operator-1", ApplicationID: repository.current.ID,
		ConfirmationCode: "contract_management", Version: repository.current.Version,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if repository.updateCalls != 0 {
		t.Fatalf("mismatched confirmation must not update, got %d calls", repository.updateCalls)
	}
}

func TestDeleteApplicationRejectsStaleVersion(t *testing.T) {
	repository := &applicationDeleteRepositoryStub{current: contractApplicationForDelete()}
	service := newApplicationDeleteService(t, repository)

	_, err := service.DeleteApplication(context.Background(), ApplicationDeleteInput{
		TenantID: repository.current.TenantID, OperatorID: "operator-1", ApplicationID: repository.current.ID,
		ConfirmationCode: repository.current.Code, Version: repository.current.Version - 1,
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	if repository.updateCalls != 0 {
		t.Fatalf("stale version must not update, got %d calls", repository.updateCalls)
	}
}
