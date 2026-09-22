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
