package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type userCreateRepositoryStub struct {
	ManagementRepository
	createUsersCalls int
	writes           []UserWrite
}

func (repository *userCreateRepositoryStub) CreateUsers(_ context.Context, writes []UserWrite) ([]domain.User, error) {
	repository.createUsersCalls++
	repository.writes = append([]UserWrite(nil), writes...)

	users := make([]domain.User, 0, len(writes))
	for _, write := range writes {
		users = append(users, domain.User{
			ID:               write.ID,
			TenantID:         write.TenantID,
			EmployeeNo:       write.EmployeeNo,
			DisplayName:      write.DisplayName,
			Email:            write.Email,
			MobileCiphertext: write.MobileCiphertext,
			Status:           write.Status,
			Version:          1,
		})
	}
	return users, nil
}

type userCreateMobileProtectionStub struct{}

func (userCreateMobileProtectionStub) Encrypt(value string) ([]byte, error) {
	return []byte("encrypted:" + value), nil
}

func (userCreateMobileProtectionStub) Decrypt(value []byte) (string, error) {
	const prefix = "encrypted:"
	if len(value) < len(prefix) || string(value[:len(prefix)]) != prefix {
		return "", errors.New("invalid encrypted test value")
	}
	return string(value[len(prefix):]), nil
}

func (userCreateMobileProtectionStub) Digest(value string) []byte {
	return []byte("digest:" + value)
}

type sequenceIDGenerator struct {
	ids   []string
	index int
}

func (generator *sequenceIDGenerator) New(time.Time) (string, error) {
	if generator.index >= len(generator.ids) {
		return "", errors.New("test ID sequence exhausted")
	}
	id := generator.ids[generator.index]
	generator.index++
	return id, nil
}

type fixedClock struct {
	now time.Time
}

func (clock fixedClock) Now() time.Time { return clock.now }

func TestManagementServiceCreateUsersBatchGeneratesManagedFields(t *testing.T) {
	t.Parallel()
	t.Skip("standalone user creation is intentionally disabled; users must be created with a membership")

	repository := &userCreateRepositoryStub{}
	ids := &sequenceIDGenerator{ids: []string{
		"01KUSER000000000000000001", "01KBINDING0000000000000001",
		"01KAPPBIND0000000000000001",
		"01KUSER000000000000000002", "01KBINDING0000000000000002",
	}}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		ids,
		fixedClock{now: time.Date(2026, time.July, 26, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	manualEmployeeNo := "CALLER-SUPPLIED-" + strings.Repeat("X", 80)
	mobile := "+86 138-0013-8000"
	views, err := service.CreateUsersBatch(context.Background(), UserBatchCreateInput{
		TenantID:   "tenant-1",
		OperatorID: "operator-1",
		Items: []UserCreateInput{
			{DisplayName: "用户甲", EmployeeNo: &manualEmployeeNo, Mobile: &mobile, Status: domain.StatusActive,
				ApplicationRoles: []ApplicationRoleAssignment{{ApplicationName: "合同管理系统", RoleName: "销售人员"}}},
			{DisplayName: "用户乙", Status: domain.StatusDisabled},
		},
	})
	if err != nil {
		t.Fatalf("create users batch: %v", err)
	}
	if repository.createUsersCalls != 1 {
		t.Fatalf("CreateUsers calls = %d, want 1", repository.createUsersCalls)
	}
	if len(repository.writes) != 2 || len(views) != 2 {
		t.Fatalf("writes/views length = %d/%d, want 2/2", len(repository.writes), len(views))
	}

	wantEmployeeNumbers := []string{
		"EMP-01KUSER000000000000000001",
		"EMP-01KUSER000000000000000002",
	}
	wantBindingIDs := []string{
		"01KBINDING0000000000000001",
		"01KBINDING0000000000000002",
	}
	for index, write := range repository.writes {
		if write.TenantID != "tenant-1" || write.OperatorID != "operator-1" {
			t.Errorf("write[%d] tenant/operator = %q/%q", index, write.TenantID, write.OperatorID)
		}
		if write.EmployeeNo == nil || *write.EmployeeNo != wantEmployeeNumbers[index] {
			t.Errorf("write[%d] employee number = %v, want %q", index, write.EmployeeNo, wantEmployeeNumbers[index])
		}
		if write.RoleBindingID != wantBindingIDs[index] {
			t.Errorf("write[%d] role binding ID = %q, want %q", index, write.RoleBindingID, wantBindingIDs[index])
		}
	}
	if got := repository.writes[0].ApplicationRoleBindings; len(got) != 1 || got[0].ID != "01KAPPBIND0000000000000001" || got[0].ApplicationName != "合同管理系统" || got[0].RoleName != "销售人员" {
		t.Fatalf("application role bindings = %#v", got)
	}
	if got := string(repository.writes[0].MobileCiphertext); got != "encrypted:+8613800138000" {
		t.Errorf("mobile ciphertext = %q", got)
	}
	if got := string(repository.writes[0].MobileHash); got != "digest:+8613800138000" {
		t.Errorf("mobile digest = %q", got)
	}
	if got := []string{*views[0].EmployeeNo, *views[1].EmployeeNo}; !reflect.DeepEqual(got, wantEmployeeNumbers) {
		t.Errorf("view employee numbers = %#v, want %#v", got, wantEmployeeNumbers)
	}
	if views[0].MobileMasked == nil || *views[0].MobileMasked != "+86****8000" {
		got := "<nil>"
		if views[0].MobileMasked != nil {
			got = *views[0].MobileMasked
		}
		t.Errorf("masked mobile = %q, want %q", got, "+86****8000")
	}
}

func TestNormalizeApplicationRoleAssignmentsSupportsNamesAndCodes(t *testing.T) {
	t.Parallel()

	got, err := normalizeApplicationRoleAssignments([]ApplicationRoleAssignment{
		{ApplicationName: "  合同管理系统 ", RoleName: " 销售人员  "},
		{ApplicationCode: "customer_management", RoleCode: "manager"},
	})
	if err != nil {
		t.Fatalf("normalize application roles: %v", err)
	}
	want := []ApplicationRoleAssignment{
		{ApplicationName: "合同管理系统", RoleName: "销售人员"},
		{ApplicationCode: "customer_management", RoleCode: "manager"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized roles = %#v, want %#v", got, want)
	}
}

func TestNormalizeApplicationRoleAssignmentsRejectsAmbiguousIdentifiers(t *testing.T) {
	t.Parallel()

	invalid := [][]ApplicationRoleAssignment{
		{{ApplicationName: "合同管理系统", RoleName: ""}},
		{{ApplicationCode: "contract_management", ApplicationName: "合同管理系统", RoleCode: "sales"}},
		{{ApplicationName: "合同管理系统", RoleCode: "sales", RoleName: "销售人员"}},
		{{ApplicationName: "合同管理系统", RoleName: "销售人员"}, {ApplicationName: "合同管理系统", RoleName: "销售人员"}},
	}
	for index, assignments := range invalid {
		if _, err := normalizeApplicationRoleAssignments(assignments); !errors.Is(err, ErrValidation) {
			t.Errorf("case %d error = %v, want ErrValidation", index, err)
		}
	}
}

func TestManagementServiceCreateUsersBatchRejectsInvalidBatchSize(t *testing.T) {
	t.Parallel()

	repository := &userCreateRepositoryStub{}
	service, err := NewManagementService(
		repository,
		userCreateMobileProtectionStub{},
		&sequenceIDGenerator{},
		fixedClock{now: time.Now()},
	)
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}

	oversized := make([]UserCreateInput, MaxBatchUserCreateItems+1)
	for index := range oversized {
		oversized[index] = UserCreateInput{DisplayName: "用户", Status: domain.StatusActive}
	}
	for name, items := range map[string][]UserCreateInput{
		"empty":     nil,
		"oversized": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			_, createErr := service.CreateUsersBatch(context.Background(), UserBatchCreateInput{
				TenantID: "tenant-1", OperatorID: "operator-1", Items: items,
			})
			if !errors.Is(createErr, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", createErr)
			}
		})
	}
	if repository.createUsersCalls != 0 {
		t.Fatalf("CreateUsers calls = %d, want 0", repository.createUsersCalls)
	}
}
