package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
)

type dictionaryTestRepository struct {
	Repository
	created DictionaryCreateInput
}

func (r *dictionaryTestRepository) CreateDictionary(_ context.Context, input DictionaryCreateInput, id string, _ time.Time) (domain.Dictionary, error) {
	r.created = input
	return domain.Dictionary{ID: id, Code: input.Code, Name: input.Name}, nil
}

type dictionaryTestID struct{}

func (dictionaryTestID) New(time.Time) (string, error) { return "dict-1", nil }

type dictionaryTestClock struct{}

func (dictionaryTestClock) Now() time.Time { return time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC) }

func TestCreateDictionaryUsesTenantScopedNormalizedCode(t *testing.T) {
	repo := &dictionaryTestRepository{}
	service, _ := NewService(repo, dictionaryTestID{}, dictionaryTestClock{})
	got, err := service.CreateDictionary(context.Background(), DictionaryCreateInput{TenantID: " tenant ", OperatorID: " user ", Code: " SERVICE_TYPE ", Name: " 服务类型 ", Status: domain.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "dict-1" || repo.created.TenantID != "tenant" || repo.created.Code != "SERVICE_TYPE" {
		t.Fatalf("got=%+v input=%+v", got, repo.created)
	}
}
func TestUpdateDictionaryRequiresOptimisticVersion(t *testing.T) {
	service, _ := NewService(&dictionaryTestRepository{}, dictionaryTestID{}, dictionaryTestClock{})
	_, err := service.UpdateDictionary(context.Background(), DictionaryUpdateInput{TenantID: "tenant", DictionaryID: "dict", OperatorID: "user", Code: "CODE", Name: "name", Status: domain.StatusActive})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want validation", err)
	}
}

type deleteTestRepository struct {
	Repository
	dictionaryInput DictionaryDeleteInput
	itemInput       ItemDeleteInput
	deletedItems    int64
}

func (r *deleteTestRepository) DeleteDictionary(_ context.Context, input DictionaryDeleteInput) (int64, error) {
	r.dictionaryInput = input
	return r.deletedItems, nil
}

func (r *deleteTestRepository) DeleteItem(_ context.Context, input ItemDeleteInput) error {
	r.itemInput = input
	return nil
}

func TestDeleteDictionaryRequiresFullContext(t *testing.T) {
	repo := &deleteTestRepository{}
	service, _ := NewService(repo, dictionaryTestID{}, dictionaryTestClock{})
	if _, err := service.DeleteDictionary(context.Background(), DictionaryDeleteInput{TenantID: "tenant", DictionaryID: "dict"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want validation", err)
	}

	if _, err := service.DeleteDictionary(context.Background(), DictionaryDeleteInput{TenantID: " tenant ", DictionaryID: " dict ", OperatorID: " user "}); err != nil {
		t.Fatal(err)
	}
	if repo.dictionaryInput.TenantID != "tenant" || repo.dictionaryInput.DictionaryID != "dict" || repo.dictionaryInput.OperatorID != "user" {
		t.Fatalf("input=%+v", repo.dictionaryInput)
	}
}

func TestDeleteDictionaryPropagatesCascadeCount(t *testing.T) {
	repo := &deleteTestRepository{deletedItems: 3}
	service, _ := NewService(repo, dictionaryTestID{}, dictionaryTestClock{})
	deleted, err := service.DeleteDictionary(context.Background(), DictionaryDeleteInput{TenantID: "tenant", DictionaryID: "dict", OperatorID: "user"})
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d error=%v", deleted, err)
	}
}

func TestDeleteItemRequiresFullContext(t *testing.T) {
	repo := &deleteTestRepository{}
	service, _ := NewService(repo, dictionaryTestID{}, dictionaryTestClock{})
	if err := service.DeleteItem(context.Background(), ItemDeleteInput{TenantID: "tenant", DictionaryID: "dict", OperatorID: "user"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want validation", err)
	}

	if err := service.DeleteItem(context.Background(), ItemDeleteInput{TenantID: "tenant", DictionaryID: "dict", ItemID: " item ", OperatorID: "user"}); err != nil {
		t.Fatal(err)
	}
	if repo.itemInput.ItemID != "item" || repo.itemInput.TenantID != "tenant" || repo.itemInput.DictionaryID != "dict" {
		t.Fatalf("input=%+v", repo.itemInput)
	}
}
