package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type employeeOnboardingRepositoryStub struct {
	ManagementRepository
	calls int
	write EmployeeOnboardingWrite
}

type employeeBatchOnboardingRepositoryStub struct {
	ManagementRepository
	called bool
	writes []EmployeeOnboardingWrite
}

func (repository *employeeBatchOnboardingRepositoryStub) CreateEmployees(_ context.Context, writes []EmployeeOnboardingWrite) ([]domain.User, error) {
	repository.called = true
	repository.writes = append([]EmployeeOnboardingWrite(nil), writes...)
	users := make([]domain.User, 0, len(writes))
	for _, write := range writes {
		users = append(users, domain.User{
			ID: write.User.ID, TenantID: write.User.TenantID, EmployeeNo: write.User.EmployeeNo,
			DisplayName: write.User.DisplayName, Email: write.User.Email,
			MobileCiphertext: write.User.MobileCiphertext, Status: write.User.Status,
			Version: 1, CreatedAt: write.OccurredAt, UpdatedAt: write.OccurredAt,
		})
	}
	return users, nil
}

func (repository *employeeOnboardingRepositoryStub) CreateEmployee(_ context.Context, write EmployeeOnboardingWrite) (domain.User, *domain.Account, *domain.Membership, error) {
	repository.calls++
	repository.write = write
	user := domain.User{
		ID: write.User.ID, TenantID: write.User.TenantID, EmployeeNo: write.User.EmployeeNo,
		DisplayName: write.User.DisplayName, Email: write.User.Email, MobileCiphertext: write.User.MobileCiphertext,
		Status: write.User.Status, Version: 1, CreatedAt: write.OccurredAt, UpdatedAt: write.OccurredAt,
	}
	var account *domain.Account
	if write.Account != nil {
		userID := write.User.ID
		account = &domain.Account{
			ID: write.Account.AccountID, TenantID: write.Account.TenantID, UserID: &userID,
			AccountName: write.Account.AccountName, AccountType: "HUMAN", AuthSource: "LOCAL",
			Status: domain.StatusActive, ValidUntil: write.Account.ValidUntil, Version: 1,
		}
	}
	var membership *domain.Membership
	if write.Membership != nil {
		membership = &domain.Membership{
			ID: write.MembershipID, TenantID: write.Membership.TenantID, MembershipType: write.Membership.MembershipType,
			EffectiveFrom: write.Membership.EffectiveFrom, EffectiveTo: write.Membership.EffectiveTo,
			Status: domain.StatusActive, Version: 1, InheritAuthorization: *write.Membership.InheritAuthorization,
		}
	}
	return user, account, membership, nil
}

type employeeOnboardingPasswordHasherStub struct{}

func (employeeOnboardingPasswordHasherStub) Hash(password string) ([]byte, []byte, error) {
	if password == "" {
		return nil, nil, errors.New("empty password")
	}
	return []byte("digest:" + password), []byte("metadata"), nil
}

func TestManagementServiceCreateEmployeeBuildsOneAtomicWrite(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	validUntil := now.Add(24 * time.Hour)
	from := now.AddDate(0, 0, 1)
	to := from.AddDate(0, 1, 0)
	inherit := true
	repository := &employeeOnboardingRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"user-1", "binding-1", "account-1", "credential-1", "membership-1"}},
		fixedClock{now: now},
		employeeOnboardingPasswordHasherStub{},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	result, err := service.CreateEmployee(context.Background(), EmployeeCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", DisplayName: "张三", Status: domain.StatusActive,
		Account:    &EmployeeAccountCreateInput{AccountName: "zhang.san", InitialPassword: "StrongPass!2026", ValidUntil: &validUntil},
		Membership: &EmployeeMembershipCreateInput{OrgUnitID: "org-1", PositionID: "position-1", MembershipType: domain.MembershipPrimary, EffectiveFrom: &from, EffectiveTo: &to, InheritAuthorization: &inherit},
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if repository.calls != 1 {
		t.Fatalf("CreateEmployee repository calls = %d, want 1", repository.calls)
	}
	if result.User.ID != "user-1" || result.User.EmployeeNo == nil || *result.User.EmployeeNo != "EMP-USER-1" {
		t.Fatalf("unexpected user result: %#v", result.User)
	}
	if result.Account == nil || result.Account.ID != "account-1" || result.Membership == nil || result.Membership.ID != "membership-1" {
		t.Fatalf("unexpected aggregate result: %#v", result)
	}
	if repository.write.User.RoleBindingID != "binding-1" || repository.write.Account == nil || repository.write.Account.CredentialID != "credential-1" {
		t.Fatalf("unexpected persistence write: %#v", repository.write)
	}
	if string(repository.write.Account.PasswordDigest) != "digest:StrongPass!2026" || string(repository.write.Account.AlgorithmParams) != "metadata" {
		t.Fatalf("password was not transformed before persistence: %#v", repository.write.Account)
	}
	if repository.write.Membership == nil || repository.write.MembershipID != "membership-1" || repository.write.Membership.InheritAuthorization == nil || !*repository.write.Membership.InheritAuthorization {
		t.Fatalf("unexpected membership write: %#v", repository.write.Membership)
	}
}

func TestManagementServiceCreateEmployeeRejectsInvalidAccountBeforeRepositoryWrite(t *testing.T) {
	t.Parallel()

	repository := &employeeOnboardingRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"user-1", "binding-1"}},
		fixedClock{now: time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)},
		employeeOnboardingPasswordHasherStub{},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	_, err = service.CreateEmployee(context.Background(), EmployeeCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", DisplayName: "张三", Status: domain.StatusActive,
		Account: &EmployeeAccountCreateInput{AccountName: "ab", InitialPassword: "StrongPass!2026"},
	})
	if !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("error = %v, want ErrMembershipRequired", err)
	}
	if repository.calls != 0 {
		t.Fatalf("repository calls = %d, want 0", repository.calls)
	}
}

func TestManagementServiceCreateEmployeesBatchDefaultsToPositionAuthorizationInheritance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 18, 10, 0, 0, 0, time.UTC)
	repository := &employeeBatchOnboardingRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"user-1", "binding-1", "account-1", "credential-1", "membership-1"}},
		fixedClock{now: now},
		employeeOnboardingPasswordHasherStub{},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	// application_roles 为空表示用户的有效应用角色必须
	// 来自所选岗位的授权模板。入职写入只需通过启用成员关系继承
	// 来保留该来源；授权查询随后会动态解析
	// POSITION/TEMPLATE 绑定。
	views, err := service.CreateEmployeesBatch(context.Background(), EmployeeBatchCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1",
		Items: []EmployeeCreateInput{{
			DisplayName: "张三", Status: domain.StatusActive,
			Membership: &EmployeeMembershipCreateInput{
				OrgUnitID: "org-1", PositionID: "position-sales", MembershipType: domain.MembershipPrimary,
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateEmployeesBatch: %v", err)
	}
	if !repository.called || len(repository.writes) != 1 || len(views.Users) != 1 {
		t.Fatalf("batch write/views = called:%t writes:%d views:%d, want true/1/1", repository.called, len(repository.writes), len(views.Users))
	}
	if len(views.InitialCredentials) != 1 || views.InitialCredentials[0].AccountName == "" || len(views.InitialCredentials[0].InitialPassword) < 12 {
		t.Fatalf("initial credentials = %#v, want one deliverable per-account password", views.InitialCredentials)
	}
	write := repository.writes[0]
	if write.Membership == nil || write.Membership.InheritAuthorization == nil || !*write.Membership.InheritAuthorization {
		t.Fatalf("membership inheritance = %#v, want explicitly enabled by default", write.Membership)
	}
	if len(write.User.ApplicationRoleBindings) != 0 {
		t.Fatalf("application role bindings = %#v, want none when application_roles is omitted", write.User.ApplicationRoleBindings)
	}
}

func TestManagementServiceCreateEmployeesBatchPreservesExplicitInheritanceOptOut(t *testing.T) {
	t.Parallel()

	inherit := false
	repository := &employeeBatchOnboardingRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{ids: []string{"user-1", "binding-1", "account-1", "credential-1", "membership-1"}},
		fixedClock{now: time.Date(2026, time.August, 18, 10, 0, 0, 0, time.UTC)},
		employeeOnboardingPasswordHasherStub{},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}
	_, err = service.CreateEmployeesBatch(context.Background(), EmployeeBatchCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1",
		Items: []EmployeeCreateInput{{
			DisplayName: "李四", Status: domain.StatusActive,
			Membership: &EmployeeMembershipCreateInput{
				OrgUnitID: "org-1", PositionID: "position-sales", MembershipType: domain.MembershipPrimary,
				InheritAuthorization: &inherit,
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateEmployeesBatch: %v", err)
	}
	if repository.writes[0].Membership == nil || repository.writes[0].Membership.InheritAuthorization == nil || *repository.writes[0].Membership.InheritAuthorization {
		t.Fatalf("membership inheritance = %#v, want explicit opt-out preserved", repository.writes[0].Membership)
	}
}

func TestImportedEmployeeInitialPasswordIsUniqueAndPolicyCompliant(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		password, err := importedEmployeeInitialPassword()
		if err != nil {
			t.Fatalf("generate password: %v", err)
		}
		if validateStrongPassword(password) != nil {
			t.Fatalf("password %q violates the strong password policy", password)
		}
		if _, duplicate := seen[password]; duplicate {
			t.Fatalf("password generator repeated a value after %d draws", i)
		}
		seen[password] = struct{}{}
	}
}
