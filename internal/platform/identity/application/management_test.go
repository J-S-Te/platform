package application

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

func TestCreateUserEncryptsAndMasksMobile(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	mobiles, err := security.NewMobileProtector(key)
	if err != nil {
		t.Fatalf("new mobile protector: %v", err)
	}
	repository := &managementRepositoryFake{}
	service, err := NewManagementService(repository, mobiles, fakeIDGenerator{}, fixedClock{now: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("new management service: %v", err)
	}
	mobile := "138-0013-8000"
	user, err := service.CreateUser(context.Background(), UserCreateInput{TenantID: "tenant", OperatorID: "operator", DisplayName: "张三", Mobile: &mobile, Status: domain.StatusActive})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.MobileMasked == nil || *user.MobileMasked != "138****8000" {
		t.Fatalf("mobile mask = %#v", user.MobileMasked)
	}
	if string(repository.write.MobileCiphertext) == "13800138000" || len(repository.write.MobileCiphertext) == 0 {
		t.Fatal("mobile was not encrypted")
	}
	if len(repository.write.MobileHash) != 32 {
		t.Fatalf("mobile hash length = %d", len(repository.write.MobileHash))
	}
}

type managementRepositoryFake struct {
	ManagementRepository
	write UserWrite
}

func (fake *managementRepositoryFake) CreateUser(_ context.Context, write UserWrite) (domain.User, error) {
	fake.write = write
	return domain.User{ID: write.ID, TenantID: write.TenantID, DisplayName: write.DisplayName, MobileCiphertext: write.MobileCiphertext, Status: write.Status, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, nil
}
