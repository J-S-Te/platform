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
	created domain.Role
	updated domain.Role
}

func (repository *roleRepositoryStub) CreateRole(_ context.Context, _, _ string, role domain.Role, _ []string) (domain.Role, error) {
	repository.created = role
	return role, nil
}

func (repository *roleRepositoryStub) UpdateRole(_ context.Context, _, _ string, role domain.Role, _ []string) (domain.Role, error) {
	repository.updated = role
	return role, nil
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
		TenantID: "tenant-1", OperatorID: "operator-1", Name: "  审计查看员  ", PermissionIDs: []string{"permission-1"},
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
}

func TestServiceUpdateRoleDoesNotAcceptCodeMutation(t *testing.T) {
	t.Parallel()

	repository := &roleRepositoryStub{}
	service, err := NewService(repository, roleIDGeneratorStub{id: "unused"}, roleClockStub{now: time.Now()})
	if err != nil {
		t.Fatalf("construct authorization service: %v", err)
	}

	_, err = service.UpdateRole(context.Background(), RoleUpdateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", RoleID: "role-1", Name: "新名称", Status: domain.StatusActive, PermissionIDs: []string{"permission-1"}, Version: 2,
	})
	if err != nil {
		t.Fatalf("update role: %v", err)
	}
	if repository.updated.Code != "" {
		t.Errorf("application update unexpectedly supplied code %q", repository.updated.Code)
	}
}
