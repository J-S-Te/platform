package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type orgUnitCreateRepositoryStub struct {
	ManagementRepository
	createCalls int
	created     domain.OrgUnit
	operatorID  string
}

func (repository *orgUnitCreateRepositoryStub) CreateOrgUnit(_ context.Context, orgUnit domain.OrgUnit, operatorID string) (domain.OrgUnit, error) {
	repository.createCalls++
	repository.created = orgUnit
	repository.operatorID = operatorID
	return orgUnit, nil
}

func TestManagementServiceCreateOrgUnitGeneratesManagedCode(t *testing.T) {
	t.Parallel()

	repository := &orgUnitCreateRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"01KYDVHC000000000000000001"}},
		fixedClock{now: time.Date(2026, time.July, 27, 8, 0, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	parentID := "parent-1"
	created, err := service.CreateOrgUnit(context.Background(), OrgUnitCreateInput{
		TenantID:   "tenant-1",
		OperatorID: "operator-1",
		ParentID:   &parentID,
		Name:       "  研发中心  ",
		SortOrder:  100,
	})
	if err != nil {
		t.Fatalf("create organization unit: %v", err)
	}

	if repository.createCalls != 1 {
		t.Fatalf("CreateOrgUnit calls = %d, want 1", repository.createCalls)
	}
	if repository.operatorID != "operator-1" {
		t.Errorf("operator ID = %q, want operator-1", repository.operatorID)
	}
	if created.ID != "01KYDVHC000000000000000001" {
		t.Errorf("organization ID = %q", created.ID)
	}
	if created.Code != "ORG-01KYDVHC000000000000000001" {
		t.Errorf("organization code = %q, want generated ORG code", created.Code)
	}
	if created.Name != "研发中心" {
		t.Errorf("organization name = %q, want trimmed name", created.Name)
	}
	if created.ParentID == nil || *created.ParentID != parentID {
		t.Errorf("parent ID = %v, want %q", created.ParentID, parentID)
	}
	if created.OrgType != "DEPARTMENT" || created.Status != domain.StatusActive || created.Version != 1 {
		t.Errorf("managed fields = type:%q status:%q version:%d", created.OrgType, created.Status, created.Version)
	}
}

func TestManagementServiceCreateOrgUnitRejectsMissingNameBeforeGeneratingID(t *testing.T) {
	t.Parallel()

	repository := &orgUnitCreateRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{},
		fixedClock{now: time.Now()},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	_, createErr := service.CreateOrgUnit(context.Background(), OrgUnitCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", Name: "  ",
	})
	if !errors.Is(createErr, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", createErr)
	}
	if repository.createCalls != 0 {
		t.Fatalf("CreateOrgUnit calls = %d, want 0", repository.createCalls)
	}
}
