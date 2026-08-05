package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/externalidentity/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/appctx"
)

type serviceRepositoryStub struct {
	provision ProvisionCommand
	role      RoleCommand
}

func (repository *serviceRepositoryStub) Provision(_ context.Context, command ProvisionCommand) (ProvisionResult, error) {
	repository.provision = command
	return ProvisionResult{PlatformUserID: command.PlatformUserID, AccountNo: command.AccountNo}, nil
}
func (repository *serviceRepositoryStub) AssignRole(_ context.Context, command RoleCommand) (domain.RoleResult, error) {
	repository.role = command
	return domain.RoleResult{PlatformUserID: command.PlatformUserID, ApplicationCode: command.ApplicationCode, RoleCode: command.RoleCode, Status: "ACTIVE"}, nil
}
func (*serviceRepositoryStub) RevokeRole(context.Context, RoleCommand) (domain.RoleResult, error) {
	return domain.RoleResult{}, errors.New("not used")
}

type mobileStub struct{}

func (mobileStub) Encrypt(value string) ([]byte, error) { return []byte("cipher:" + value), nil }
func (mobileStub) Digest(value string) []byte {
	result := make([]byte, 32)
	copy(result, value)
	return result
}

type idsStub struct{ values []string }

func (generator *idsStub) New(time.Time) (string, error) {
	value := generator.values[0]
	generator.values = generator.values[1:]
	return value, nil
}

type clockStub struct{ now time.Time }

func (clockStub clockStub) Now() time.Time { return clockStub.now }

func principal() appctx.Principal {
	return appctx.Principal{OAuthClientID: "oauth-1", ClientID: "crm-external", TenantID: "tenant-a", ApplicationID: "crm-app", ApplicationCode: "crm", EnvironmentID: "env-1", EnvironmentCode: "prod", Scopes: map[string]struct{}{"external_user.provision": {}}}
}

func TestProvisionNormalizesAndNeverCreatesCredentialInput(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{}
	service, err := NewService(repository, mobileStub{}, &idsStub{values: []string{"identity-1", "user-1", "event-1", "account-1"}}, clockStub{now: now})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Provision(context.Background(), principal(), RequestProof{IdempotencyKey: " key-1 ", Timestamp: now, Nonce: "nonce-1"}, ProvisionInput{DisplayName: " Customer User ", Mobile: "+86 138-0000-0000", Email: "USER@EXAMPLE.COM"})
	if err != nil {
		t.Fatal(err)
	}
	if result.PlatformUserID != "user-1" || repository.provision.AccountID != "account-1" || repository.provision.DisplayName != "Customer User" || *repository.provision.Email != "user@example.com" || string(repository.provision.MobileCipher) != "cipher:+8613800000000" {
		t.Fatalf("result=%+v command=%+v", result, repository.provision)
	}
	if repository.provision.IdempotencyKey != "key-1" {
		t.Fatalf("idempotency key=%q", repository.provision.IdempotencyKey)
	}
}

func TestPortalApplicationCodeCanBeOverridden(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{}
	service, err := NewService(repository, mobileStub{}, &idsStub{values: []string{"binding-1", "event-1"}}, clockStub{now: now}, WithPortalApplicationCode("custom_portal"))
	if err != nil {
		t.Fatal(err)
	}
	// 覆盖后，平台默认门户应用码 customer_portal 不再被接受（B4 解耦：门户应用码可配置）。
	if _, err := service.AssignPortalRole(context.Background(), principal(), RequestProof{IdempotencyKey: "key", Timestamp: now, Nonce: "nonce"}, RoleInput{PlatformUserID: "user", ApplicationCode: PortalApplicationCode, RoleCode: PortalCustomerRole}); !errors.Is(err, ErrValidation) {
		t.Fatalf("default portal code should be rejected after override, error=%v", err)
	}
}

func TestProofAndPortalRoleFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{}
	service, err := NewService(repository, mobileStub{}, &idsStub{values: []string{"binding-1", "event-1"}}, clockStub{now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.AssignPortalRole(context.Background(), principal(), RequestProof{IdempotencyKey: "key", Timestamp: now.Add(-6 * time.Minute), Nonce: "nonce"}, RoleInput{PlatformUserID: "user", ApplicationCode: PortalApplicationCode, RoleCode: PortalCustomerRole}); !errors.Is(err, ErrReplay) {
		t.Fatalf("stale timestamp error=%v", err)
	}
	if _, err = service.AssignPortalRole(context.Background(), principal(), RequestProof{IdempotencyKey: "key", Timestamp: now, Nonce: "nonce"}, RoleInput{PlatformUserID: "user", ApplicationCode: "another-app", RoleCode: PortalCustomerRole}); !errors.Is(err, ErrValidation) {
		t.Fatalf("cross-application role error=%v", err)
	}
}
