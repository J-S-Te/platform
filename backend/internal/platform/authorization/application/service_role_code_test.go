package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/domain"
)

type roleRepositoryStub struct {
	Repository
	created                 domain.Role
	updated                 domain.Role
	createOperatorAccountID string
	updateOperatorAccountID string
	roleBindingCalls        int
}

func (repository *roleRepositoryStub) CreateRole(_ context.Context, _, _, operatorAccountID string, role domain.Role, _ []string) (domain.Role, error) {
	repository.created = role
	repository.createOperatorAccountID = operatorAccountID
	return role, nil
}

func (repository *roleRepositoryStub) UpdateRole(_ context.Context, _, _, operatorAccountID string, role domain.Role, _ []string) (domain.Role, error) {
	repository.updated = role
	repository.updateOperatorAccountID = operatorAccountID
	return role, nil
}

func (repository *roleRepositoryStub) CreateRoleBinding(_ context.Context, _, _ string, binding domain.RoleBinding) (domain.RoleBinding, error) {
	repository.roleBindingCalls++
	return binding, nil
}

type roleIDGeneratorStub struct{ id string }

func (generator roleIDGeneratorStub) New(time.Time) (string, error) {
	if generator.id == "" {
		return "", errors.New("missing test ID")
	}
	return generator.id, nil
}

type roleClockStub struct{ now time.Time }

func (clock roleClockStub) Now() time.Time { return clock.now }

func TestServiceCreateRoleGeneratesManagedCode(t *testing.T) {
	t.Parallel()

	repository := &roleRepositoryStub{}
	service, err := NewService(repository, roleIDGeneratorStub{id: "01KYDVHC000000000000000003"}, roleClockStub{now: time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("construct authorization service: %v", err)
	}

	created, err := service.CreateRole(context.Background(), RoleCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", OperatorAccountID: "account-1", Name: "  审计查看员  ", PermissionIDs: []string{"permission-1"},
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if created.Code != "ROLE-01KYDVHC000000000000000003" {
		t.Errorf("role code = %q, want managed ROLE code", created.Code)
	}
	if created.Name != "审计查看员" || created.Status != domain.StatusActive {
		t.Errorf("created role = %#v", created)
	}
	if repository.createOperatorAccountID != "account-1" {
		t.Errorf("create operator account ID = %q, want account-1", repository.createOperatorAccountID)
	}
}

func TestServiceUpdateRoleDoesNotAcceptCodeMutation(t *testing.T) {
	t.Parallel()

	repository := &roleRepositoryStub{}
	service, err := NewService(repository, roleIDGeneratorStub{id: "unused"}, roleClockStub{now: time.Now()})
	if err != nil {
		t.Fatalf("construct authorization service: %v", err)
	}

	_, err = service.UpdateRole(context.Background(), RoleUpdateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", OperatorAccountID: "account-1", RoleID: "role-1", Name: "新名称", Status: domain.StatusActive, PermissionIDs: []string{"permission-1"}, Version: 2,
	})
	if err != nil {
		t.Fatalf("update role: %v", err)
	}
	if repository.updated.Code != "" {
		t.Errorf("application update unexpectedly supplied code %q", repository.updated.Code)
	}
	if repository.updateOperatorAccountID != "account-1" {
		t.Errorf("update operator account ID = %q, want account-1", repository.updateOperatorAccountID)
	}
}

func TestServiceRoleBindingAcceptsEnforcedOrganizationScope(t *testing.T) {
	t.Parallel()

	repository := &roleRepositoryStub{}
	service, err := NewService(repository, roleIDGeneratorStub{id: "01KYDVHC000000000000000003"}, roleClockStub{now: time.Date(2026, time.July, 28, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("construct authorization service: %v", err)
	}
	scopeID := "org-1"
	_, err = service.CreateRoleBinding(context.Background(), RoleBindingCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", RoleID: "role-1", SubjectType: "USER", SubjectID: "user-1", ScopeType: "ORG_UNIT", ScopeID: &scopeID,
	})
	if err != nil {
		t.Fatalf("create organization-scoped binding: %v", err)
	}
	if repository.roleBindingCalls != 1 {
		t.Fatalf("CreateRoleBinding calls = %d, want 1", repository.roleBindingCalls)
	}
}
