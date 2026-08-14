package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/domain"
)

type customerRefStub struct {
	encryptErr error
	decryptErr error
	digestErr  error
	digestCalls []string
}

func (stub *customerRefStub) Encrypt(value string) ([]byte, error) {
	if stub.encryptErr != nil {
		return nil, stub.encryptErr
	}
	return []byte("ct:" + value), nil
}

func (stub *customerRefStub) Decrypt(ciphertext []byte) (string, error) {
	if stub.decryptErr != nil {
		return "", stub.decryptErr
	}
	return strings.TrimPrefix(string(ciphertext), "ct:"), nil
}

func (stub *customerRefStub) Digest(value string) ([]byte, error) {
	stub.digestCalls = append(stub.digestCalls, value)
	if stub.digestErr != nil {
		return nil, stub.digestErr
	}
	digest := sha256.Sum256([]byte(value))
	return digest[:], nil
}

func TestBindCustomerComputesProtectedBinding(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{}
	protection := &customerRefStub{}
	service, err := NewService(repository, mobileStub{}, &idsStub{values: []string{"binding-1", "event-1"}}, clockStub{now: now}, WithCustomerRefProtection(protection))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.BindCustomer(context.Background(), principal(), RequestProof{IdempotencyKey: " key-1 ", Timestamp: now, Nonce: "nonce-1"}, BindInput{PlatformUserID: " user ", CustomerRef: " CRM-CUST-1 "})
	if err != nil {
		t.Fatalf("BindCustomer() error = %v", err)
	}
	if result.PlatformUserID != "user" || result.ApplicationCode != PortalApplicationCode || result.Status != domain.BindingActive {
		t.Fatalf("BindCustomer() result = %#v", result)
	}
	command := repository.binding
	if command.PlatformUserID != "user" || command.ApplicationCode != PortalApplicationCode || command.Status != domain.BindingActive ||
		command.CustomerRefDigest == nil || command.CustomerRefCipher == nil || string(command.CustomerRefCipher) != "ct:CRM-CUST-1" {
		t.Fatalf("BindCustomer() command = %#v", command)
	}
	expectedInput := "customer\x00tenant-a\x00" + PortalApplicationCode + "\x00CRM-CUST-1"
	if len(protection.digestCalls) != 1 || protection.digestCalls[0] != expectedInput {
		t.Fatalf("digest inputs = %#v, want %q", protection.digestCalls, expectedInput)
	}
}

func TestDisableCustomerBindingSkipsCiphertext(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{}
	service, err := NewService(repository, mobileStub{}, &idsStub{values: []string{"binding-1", "event-1"}}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.DisableCustomerBinding(context.Background(), principal(), RequestProof{IdempotencyKey: "key-1", Timestamp: now, Nonce: "nonce-1"}, BindInput{PlatformUserID: "user", CustomerRef: "CRM-CUST-1"})
	if err != nil {
		t.Fatalf("DisableCustomerBinding() error = %v", err)
	}
	if result.Status != domain.BindingDisabled || repository.binding.CustomerRefCipher != nil || repository.binding.CustomerRefDigest == nil {
		t.Fatalf("DisableCustomerBinding() result = %#v command = %#v", result, repository.binding)
	}
}

func TestBindCustomerValidationAndProtectionErrors(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	service, err := NewService(&serviceRepositoryStub{}, mobileStub{}, &idsStub{values: []string{"b", "e"}}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BindCustomer(context.Background(), principal(), RequestProof{IdempotencyKey: "k", Timestamp: now, Nonce: "n"}, BindInput{PlatformUserID: "user", CustomerRef: "  "}); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank customer ref error = %v, want ErrValidation", err)
	}
	unavailable, _ := NewService(&serviceRepositoryStub{}, mobileStub{}, &idsStub{values: []string{"b", "e"}}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{digestErr: errors.New("no key")}))
	if _, err := unavailable.BindCustomer(context.Background(), principal(), RequestProof{IdempotencyKey: "k", Timestamp: now, Nonce: "n"}, BindInput{PlatformUserID: "user", CustomerRef: "CRM-CUST-1"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("digest failure error = %v, want ErrUnavailable", err)
	}
}

func TestResolveCustomerBinding(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{bindingRecord: domain.CustomerBinding{ApplicationCode: PortalApplicationCode, CustomerRefCipher: []byte("ct:CRM-CUST-1"), Status: domain.BindingActive}}
	service, err := NewService(repository, mobileStub{}, &idsStub{}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{}))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveCustomerBinding(context.Background(), "tenant-a", "user", PortalApplicationCode)
	if err != nil || resolved.CustomerRef != "CRM-CUST-1" {
		t.Fatalf("ResolveCustomerBinding() = %#v, %v", resolved, err)
	}
	if repository.query.Status != domain.BindingActive {
		t.Fatalf("query status = %q, want ACTIVE", repository.query.Status)
	}

	// 禁用绑定不产生声明。
	repository.bindingRecord.Status = domain.BindingDisabled
	if _, err := service.ResolveCustomerBinding(context.Background(), "tenant-a", "user", PortalApplicationCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled binding error = %v, want ErrNotFound", err)
	}
	// 应用码不属于门户时按未找到处理。
	if _, err := service.ResolveCustomerBinding(context.Background(), "tenant-a", "user", "contract_management"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign application error = %v, want ErrNotFound", err)
	}
}

func TestResolveCustomerBindingDecryptFailureIsUnavailable(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{bindingRecord: domain.CustomerBinding{ApplicationCode: PortalApplicationCode, CustomerRefCipher: []byte("ct:x"), Status: domain.BindingActive}}
	service, err := NewService(repository, mobileStub{}, &idsStub{}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{decryptErr: errors.New("bad ciphertext")}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveCustomerBinding(context.Background(), "tenant-a", "user", PortalApplicationCode); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("decrypt failure error = %v, want ErrUnavailable", err)
	}
}

func TestCustomerBindingStatus(t *testing.T) {
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repository := &serviceRepositoryStub{bindingRecord: domain.CustomerBinding{ApplicationCode: PortalApplicationCode, Status: domain.BindingDisabled}}
	service, err := NewService(repository, mobileStub{}, &idsStub{}, clockStub{now: now}, WithCustomerRefProtection(&customerRefStub{}))
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.CustomerBindingStatus(context.Background(), "tenant-a", "user", PortalApplicationCode)
	if err != nil || status.Status != domain.BindingDisabled {
		t.Fatalf("CustomerBindingStatus() = %#v, %v", status, err)
	}
	if repository.query.Status != "" {
		t.Fatalf("status query filtered unexpectedly: %q", repository.query.Status)
	}
}
