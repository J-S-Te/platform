package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

type positionMembershipRepositoryStub struct {
	ManagementRepository
	createdPosition  domain.Position
	positionOperator string
	membershipCalls  int
	deletedPosition  PositionDeleteInput
}

func (repository *positionMembershipRepositoryStub) CreatePosition(_ context.Context, position domain.Position, operatorID string) (domain.Position, error) {
	repository.createdPosition = position
	repository.positionOperator = operatorID
	return position, nil
}

func (repository *positionMembershipRepositoryStub) DeletePosition(_ context.Context, input PositionDeleteInput) error {
	repository.deletedPosition = input
	return nil
}

func (repository *positionMembershipRepositoryStub) CreateMembership(_ context.Context, input MembershipCreateInput, membershipID string) (domain.Membership, error) {
	repository.membershipCalls++
	return domain.Membership{ID: membershipID, TenantID: input.TenantID, MembershipType: input.MembershipType, EffectiveFrom: input.EffectiveFrom, EffectiveTo: input.EffectiveTo}, nil
}

func TestManagementServiceCreatePositionGeneratesManagedCode(t *testing.T) {
	t.Parallel()

	repository := &positionMembershipRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"01KYDVHC000000000000000002"}},
		fixedClock{now: time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	created, err := service.CreatePosition(context.Background(), PositionCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", OrgUnitID: "org-1", Name: "  研发经理  ",
	})
	if err != nil {
		t.Fatalf("create position: %v", err)
	}
	if created.Code != "POS-01KYDVHC000000000000000002" {
		t.Errorf("position code = %q, want managed POS code", created.Code)
	}
	if created.Name != "研发经理" || created.OrgUnitID != "org-1" {
		t.Errorf("created position = %#v", created)
	}
	if created.Status != domain.StatusActive || created.Version != 1 {
		t.Errorf("managed fields = status:%q version:%d", created.Status, created.Version)
	}
	if repository.positionOperator != "operator-1" {
		t.Errorf("operator ID = %q", repository.positionOperator)
	}
}

func TestManagementServiceMembershipRequiresCompleteShortTermRange(t *testing.T) {
	t.Parallel()

	repository := &positionMembershipRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"membership-long", "membership-short"}},
		fixedClock{now: time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	base := MembershipCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", UserID: "user-1", OrgUnitID: "org-1", PositionID: "position-1", MembershipType: domain.MembershipPrimary,
	}
	if _, err := service.CreateMembership(context.Background(), base); err != nil {
		t.Fatalf("create long-term membership: %v", err)
	}

	from := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC)
	shortTerm := base
	shortTerm.EffectiveFrom = &from
	shortTerm.EffectiveTo = &to
	if _, err := service.CreateMembership(context.Background(), shortTerm); err != nil {
		t.Fatalf("create short-term membership: %v", err)
	}

	missingEnd := base
	missingEnd.EffectiveFrom = &from
	if _, err := service.CreateMembership(context.Background(), missingEnd); !errors.Is(err, ErrValidation) {
		t.Fatalf("one-sided range error = %v, want ErrValidation", err)
	}

	reversed := base
	reversed.EffectiveFrom = &to
	reversed.EffectiveTo = &from
	if _, err := service.CreateMembership(context.Background(), reversed); !errors.Is(err, ErrValidation) {
		t.Fatalf("reversed range error = %v, want ErrValidation", err)
	}

	if repository.membershipCalls != 2 {
		t.Fatalf("CreateMembership calls = %d, want 2 valid writes", repository.membershipCalls)
	}
}

func TestManagementServiceDeletePositionRequiresVersionAndDelegates(t *testing.T) {
	t.Parallel()

	repository := &positionMembershipRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{},
		fixedClock{now: time.Date(2026, time.July, 30, 9, 0, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	if err := service.DeletePosition(context.Background(), PositionDeleteInput{
		TenantID: "tenant-1", OperatorID: "operator-1", PositionID: "position-1",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing version error = %v, want ErrValidation", err)
	}

	input := PositionDeleteInput{TenantID: "tenant-1", OperatorID: "operator-1", PositionID: "position-1", Version: 3}
	if err := service.DeletePosition(context.Background(), input); err != nil {
		t.Fatalf("delete position: %v", err)
	}
	if repository.deletedPosition != input {
		t.Fatalf("delegated input = %#v, want %#v", repository.deletedPosition, input)
	}
}
