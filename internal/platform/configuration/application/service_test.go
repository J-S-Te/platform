package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/domain"
)

type configTestRepository struct {
	Repository
	created NamespaceCreateInput
}

func (r *configTestRepository) CreateNamespace(_ context.Context, input NamespaceCreateInput, id string, _ time.Time) (domain.Namespace, error) {
	r.created = input
	return domain.Namespace{ID: id, Code: input.Code, Name: input.Name}, nil
}

type configTestID struct{}

func (configTestID) New(time.Time) (string, error) { return "ns-1", nil }

type configTestClock struct{}

func (configTestClock) Now() time.Time { return time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC) }

func TestCreateNamespaceNormalizesAndPersists(t *testing.T) {
	repo := &configTestRepository{}
	service, err := NewService(repo, configTestID{}, configTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CreateNamespace(context.Background(), NamespaceCreateInput{TenantID: " tenant ", OperatorID: " user ", ApplicationCode: "CRM", Code: " DEFAULT ", Name: " 默认配置 "})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "ns-1" || repo.created.TenantID != "tenant" || repo.created.Code != "DEFAULT" {
		t.Fatalf("unexpected result=%+v input=%+v", result, repo.created)
	}
}

func TestCreateItemRejectsSecretWithoutSecretStore(t *testing.T) {
	service, _ := NewService(&configTestRepository{}, configTestID{}, configTestClock{})
	_, err := service.CreateItem(context.Background(), ItemCreateInput{TenantID: "tenant", OperatorID: "user", NamespaceID: "ns", Key: "password", ValueType: "STRING", Value: "secret", Secret: true})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want validation", err)
	}
}
